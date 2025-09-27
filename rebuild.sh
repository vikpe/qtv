#!/bin/bash

QTVDIR="pkg"   # directory to watch
QTVCMD="./qtv-go"
PIDFILE="/tmp/qtv.pid"

while inotifywait -e modify,create,delete -r "$QTVDIR"; do
    # stop existing qtv if running
    if [ -f "$PIDFILE" ] && kill -0 $(cat "$PIDFILE") 2>/dev/null; then
        kill $(cat "$PIDFILE")
        wait $(cat "$PIDFILE") 2>/dev/null
    fi

    # rebuild
    make build

    # run qtv in background and save PID
    $QTVCMD &
    echo $! > "$PIDFILE"
done

