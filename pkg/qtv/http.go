package qtv

import (
	_ "embed"
	"fmt"
	"html/template"
	"io/fs"
	"io/ioutil"
	stdlog "log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/adam-lavrik/go-imath/ix"
	"github.com/gorilla/mux"
	"github.com/hashicorp/go-multierror"
	"github.com/rs/zerolog/log"
	"github.com/soheilhy/cmux"
	"go.uber.org/atomic"
)

//
// HTML templates
//

//go:embed html_templates/layout.html
var templateLayout string

//go:embed html_templates/servers.html
var templateServers string

//go:embed html_templates/demos.html
var templateDemos string

//
// Built-in HTTP interface.
//

type httpSv struct {
	qtv             *QTV               // Parent object.
	layoutTemplate  *template.Template // Layout template.
	demosTemplate   *template.Template // Demos html template, using layout.
	serversTemplate *template.Template // Servers html template, using layout.
	upload          atomic.Bool        // True if upload is active.
	lastUpload      time.Time          // Time of last upload start.
}

func newHttpSv(qtv *QTV) *httpSv {
	sv := &httpSv{
		qtv: qtv,
	}

	sv.regVars(qtv)

	return sv
}

func (sv *httpSv) regVars(qtv *QTV) {
	qtv.qvs.RegEx("http_enabled", "1", qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_readtimeout", "45", qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_writetimeout", "600", qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_idletimeout", "60", qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_upload_enabled", "1", qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_upload_total_limit", 1024*1024*64, qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_upload_file_limit", 1024*1024*32, qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_server_cert_file", "", qVarFlagInitOnly, nil)
	qtv.qvs.RegEx("http_server_key_file", "", qVarFlagInitOnly, nil)
}

func (sv *httpSv) isEnabled() bool {
	return sv.qtv.qvs.Get("http_enabled").Bool
}

// Limit is up to 60 seconds.
func (sv *httpSv) readTimeOut() time.Duration {
	return durationBound(1, sv.qtv.qvs.Get("http_readtimeout").Dur, 60) * time.Second
}

// Limit is up to 15 minutes.
func (sv *httpSv) writeTimeOut() time.Duration {
	return durationBound(1, sv.qtv.qvs.Get("http_writetimeout").Dur, 60*15) * time.Second
}

// Limit is up to 60 seconds.
func (sv *httpSv) idleTimeOut() time.Duration {
	return durationBound(1, sv.qtv.qvs.Get("http_idletimeout").Dur, 60) * time.Second
}

func (sv *httpSv) uploadEnabled() bool {
	return sv.qtv.qvs.Get("http_upload_enabled").Bool
}

func (sv *httpSv) uploadTotalLimit() int64 {
	return i64Bound(1024*1024*1, int64(sv.qtv.qvs.Get("http_upload_total_limit").Float), 1024*1024*1024*2)
}

func (sv *httpSv) uploadFileLimit() int64 {
	return i64Bound(1024*1024*1, int64(sv.qtv.qvs.Get("http_upload_file_limit").Float), 1024*1024*128)
}

type mainTemplateData struct {
	Title      string
	HelpURL    string
	ProjectURL string
	Version    string
	Build      string
	HostName   string // QTV hostname.
	Address    string
}

type demosTemplateData struct {
	mainTemplateData
	List demoList
}

type nowPlayingTemplateData struct {
	mainTemplateData
	List []*uStreamInfo
}

// Get base data required for main template.
func (sv *httpSv) getMainTemplateData(r *http.Request, title string) mainTemplateData {
	data := mainTemplateData{
		Title:      "QuakeTV: " + title,
		HelpURL:    qtvHelpURL,
		ProjectURL: qtvProjectURL,
		Version:    qtvRelease,
		Build:      qtvBuild,
		HostName:   sv.qtv.hostName(),
		Address:    sv.qtv.qvs.Get("address").Str,
	}
	if data.Address == "" {
		data.Address = r.Host
	}
	return data
}

// Convert bytes to kilobytes.
func toKb(v int64) int64 {
	return v / 1024
}

// Helps to mark rows as even/odd during html generation.
func isEven(i int) string {
	if (i & 1) != 0 {
		return "odd"
	}
	return "even"
}

var (
	// Provide our custom functions for HTML template processor.
	qtvTemplateFuncs = template.FuncMap{
		"toKb":      toKb,
		"isEven":    isEven,
		"hasSuffix": strings.HasSuffix,
	}
)

// Prepare HTTP server (parse HTML templates).
func (sv *httpSv) prepare() (err error) {
	defer func() { err = multierror.Prefix(err, "httpSv.prepare:") }()

	sv.layoutTemplate, err = template.New("qtvMain").Funcs(qtvTemplateFuncs).Parse(templateLayout)
	if err != nil {
		return err
	}

	sv.demosTemplate, err = sv.layoutTemplate.Clone()
	if err != nil {
		return err
	}
	_, err = sv.demosTemplate.Parse(templateDemos)
	if err != nil {
		return err
	}

	sv.serversTemplate, err = sv.layoutTemplate.Clone()
	if err != nil {
		return err
	}
	_, err = sv.serversTemplate.Parse(templateServers)
	if err != nil {
		return err
	}

	return nil
}

func sanitizeUploadFileName(name string) string {
	b := []byte(strings.TrimSuffix(name, ".mvd"))
	b = b[:ix.Min(len(b), 128)] // Truncate name.

	for i := 0; i < len(b); i++ {
		r := rune(b[i])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '[' || r == ']' {
			continue
		}
		b[i] = '_'
	}

	return string(b)
}

func (sv *httpSv) uploadFile(w http.ResponseWriter, r *http.Request) {
	if !sv.uploadEnabled() {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "Upload is not allowed\n")
		return
	}

	// Add some limitations for upload so it not so easy to abuse it.
	if !sv.upload.CAS(false, true) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "Only one upload allowed simultaneously\n")
		return
	}
	defer func() { sv.upload.CAS(true, false) }()

	if time.Now().Sub(sv.lastUpload) < 1*time.Minute {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "Only one upload allowed per minute\n")
		return
	}
	sv.lastUpload = time.Now()

	// Limit upload size of one file by 32 megabytes.
	r.Body = http.MaxBytesReader(w, r.Body, sv.uploadFileLimit())
	// FormFile returns the first file for the given key `file`
	// it also returns the FileHeader so we can get the Filename, the Header and the Size of the file.
	file, handler, err := r.FormFile("file")
	if err != nil {
		log.Debug().Err(multierror.Prefix(err, "httpSv.uploadFile:")).Str("ctx", "httpSv").Msg("")
		return
	}
	defer file.Close()

	fileName := strings.ToLower(handler.Filename)
	if filepath.Ext(fileName) != ".mvd" {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "Invalid upload file extension, only mvd files supported\n")
		return
	}

	fileName = sanitizeUploadFileName(fileName)

	// Read all of the contents of our uploaded file into a byte array. FIXME: bad idea since it use a lot of RAM, better use io.Copy().
	fileBytes, err := ioutil.ReadAll(file)
	if err != nil {
		log.Debug().Err(multierror.Prefix(err, "httpSv.uploadFile:")).Str("ctx", "httpSv").Msg("")
		return
	}

	// Minor validation if it really a MVD file.
	validationLen := ix.Min(len(fileBytes), 1024*100)
	if _, ms := consistantMVD(fileBytes[:validationLen], false); ms < 500 {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "Invalid upload file, only mvd files supported\n")
		return
	}

	// Create a temporary file within our demo directory that follows a particular naming pattern.
	tempFile, err := ioutil.TempFile(sv.qtv.demoDir(), "upload-*-"+fileName+".mvd")
	if err != nil {
		log.Debug().Err(multierror.Prefix(err, "httpSv.uploadFile:")).Str("ctx", "httpSv").Msg("")
		return
	}
	defer tempFile.Close()

	// Write this byte array to our temporary file.
	if _, err := tempFile.Write(fileBytes); err != nil {
		log.Debug().Err(multierror.Prefix(err, "httpSv.uploadFile:")).Str("ctx", "httpSv").Msg("")
		os.Remove(tempFile.Name())
		return
	}
	// Return that we have successfully uploaded our file.
	log.Trace().Str("ctx", "httpSv").Str("event", "uploadFile").Str("file", tempFile.Name()).Int64("size", handler.Size).Msg("")
	fmt.Fprintf(w, "Successfully uploaded file as %v\n", tempFile.Name())
}

func (sv *httpSv) demosHandler(w http.ResponseWriter, r *http.Request) {
	data := demosTemplateData{
		mainTemplateData: sv.getMainTemplateData(r, "Demos"),
		List:             sv.qtv.getDemoList(),
	}

	if err := sv.demosTemplate.Execute(w, data); err != nil {
		log.Debug().Err(multierror.Prefix(err, "httpSv.demosHandler:")).Str("ctx", "httpSv").Msg("")
	}
}

func (sv *httpSv) demosHandlerCompat(w http.ResponseWriter, r *http.Request) {
	demoList := sv.qtv.getDemoList()
	hashFunction := r.URL.Query().Get("hash")

	for _, demo := range demoList {
		var line string

		switch hashFunction {
		case "xxh3":
			line = fmt.Sprintf("%s %s", demo.Hash.XXH3, demo.FileInfo.Name())
		default:
			line = demo.FileInfo.Name()
		}

		if _, err := fmt.Fprintln(w, line); err != nil {
			log.Debug().Err(multierror.Prefix(err, "httpSv.demosHandlerCompat: failed to write demolist")).
				Str("ctx", "httpSv").
				Str("filename", demo.FileInfo.Name()).
				Str("hash", hashFunction).
				Msg("")
			break
		}
	}
}

func (sv *httpSv) nowPlayingHandler(w http.ResponseWriter, r *http.Request) {
	data := nowPlayingTemplateData{
		mainTemplateData: sv.getMainTemplateData(r, "Now Playing"),
		List:             sv.qtv.uss.getUStreamInfo(),
	}
	// Sort stream list by id so page looks similar on each load.
	sort.Slice(data.List, func(i, j int) bool { return data.List[i].Id < data.List[j].Id })

	if err := sv.serversTemplate.Execute(w, data); err != nil {
		log.Debug().Err(multierror.Prefix(err, "httpSv.nowHandler:")).Str("ctx", "httpSv").Msg("")
	}
}

// Returns true if file name starts with dot.
func hiddenFile(name string) bool {
	return strings.HasPrefix(name, ".")
}

// containsHiddenFile reports whether name contains a path element starting with a period.
// The name is assumed to be a delimited by forward slashes, as guaranteed
// by the http.FileSystem interface.
func containsHiddenFile(name string) bool {
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if hiddenFile(part) {
			return true
		}
	}
	return false
}

// hidingFile is the http.File use in fileHidingFileSystem.
// It is used to wrap the Readdir method of http.File so that we can
// remove files and directories that start with a period from its output.
type hidingFile struct {
	http.File
}

// Readdir is a wrapper around the Readdir method of the embedded File
// that filters out all files that start with a period in their name.
func (f hidingFile) Readdir(n int) (fis []fs.FileInfo, err error) {
	files, err := f.File.Readdir(n)
	// Filters out not allowed files.
	for _, file := range files {
		name := file.Name()
		if hiddenFile(name) || fileNameHasSensitiveExtension(name) {
			continue
		}

		fis = append(fis, file)
	}
	return
}

// fileHidingFileSystem is an http.FileSystem that hides
// hidden/sensitive files from being served.
type fileHidingFileSystem struct {
	http.FileSystem
}

// Open is a wrapper around the Open method of the embedded FileSystem
// that serves a 403 permission error when name has a file or directory
// with whose name starts with a period in its path.
func (fsys fileHidingFileSystem) Open(name string) (http.File, error) {
	// If sensitive file, return 403 response
	if containsHiddenFile(name) || fileNameHasSensitiveExtension(name) {
		return nil, fs.ErrPermission
	}

	file, err := fsys.FileSystem.Open(name)
	if err != nil {
		return nil, err
	}
	return hidingFile{file}, err
}

// Serve HTTP(s) requests.
func (sv *httpSv) serve(l net.Listener) (err error) {
	r := mux.NewRouter()

	r.HandleFunc("/", sv.nowPlayingHandler)
	r.HandleFunc("/demos/", sv.demosHandler)
	r.HandleFunc("/upload/", sv.uploadFile)

	// Compat with original QTV
	r.HandleFunc("/demo_filenames", sv.demosHandlerCompat)
	r.HandleFunc("/dl/demos/{file:.*}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/demos/"+url.PathEscape(mux.Vars(r)["file"]), http.StatusMovedPermanently)
	})

	// File server for demo dir.
	demosFileSys := fileHidingFileSystem{http.Dir(sv.qtv.demoDir())}
	r.PathPrefix("/demos/").Handler(http.StripPrefix("/demos/", http.FileServer(demosFileSys)))

	// File server for qtv dir.
	// Would be better to have such files inside qtv/httproot but for backward compatibility we host whole directory.
	// We hide .cfg and .dot files though.
	qtvFileSys := fileHidingFileSystem{http.Dir("public")}
	r.PathPrefix("/").Handler(http.FileServer(qtvFileSys))

	// Replace stdlog with zerolog inside http server.
	stdLog := stdlog.New(nil, "", 0)
	stdLog.SetFlags(0)
	stdLog.SetOutput(log.Logger)

	s := &http.Server{
		Handler:  r,
		ErrorLog: stdLog,
		// It is overall timeout for write,
		// should be quite huge so client with slow connection has a chance to download data.
		WriteTimeout: sv.writeTimeOut(),
		ReadTimeout:  sv.readTimeOut(),
		IdleTimeout:  sv.idleTimeOut(),
	}
	certFile := sv.qtv.qvs.Get("http_server_cert_file").Str
	keyFile := sv.qtv.qvs.Get("http_server_key_file").Str
	isTls := certFile != "" && keyFile != ""
	if isTls {
		err = s.ServeTLS(l, certFile, keyFile)
	} else {
		err = s.Serve(l)
	}
	// Ensure QTV is stopping if HTTP server got error of some kind.
	// This mostly required for the case when ServeTLS() could not find cert/key file.
	if err != cmux.ErrServerClosed {
		sv.qtv.Stop()
	}
	return err
}
