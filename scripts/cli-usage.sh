#!/bin/sh
# cli-usage.sh COMMAND... prints the help of `talaria COMMAND...` without the
# global flags, for the generated CLI reference (docs/reference/cli.md). The
# default of --name is the host name of this computer, so it is replaced.
set -eu
go run . "$@" -h 2>&1 |
	sed -e '/^Global Flags:/,$d' -e '/--name string/s/(default "[^"]*")/(default the host name)/' |
	sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
