#! /bin/sh

# make sure to set APPDIR when run directly from the AppDir
if [ -z $APPDIR ]; then APPDIR=$(readlink -f $(dirname "$0")); fi

export LD_LIBRARY_PATH="$APPDIR"/usr/lib
exec "$APPDIR/usr/bin/cells-sync" "$@"
