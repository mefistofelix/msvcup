#!/usr/bin/env bash

# EXTERNAL COMMANDS USED BY THIS BUILD SCRIPT: wget mkdir tar sha256sum

# Determine the absolute directory containing this script using only Bash.
get_script_dir() {
  local working_dir
  local script_path

  working_dir="$PWD"
  [ "$PWD" = "/" ] && working_dir=""
  case "$0" in
    /*) script_path="$0" ;;
    *) script_path="$working_dir/${0#./}" ;;
  esac
  REPLY="${script_path%/*}"
}

get_script_dir
script_dir=$REPLY
project_name=${script_dir##*/}

go_url="https://go.dev/dl/go1.26.5.linux-amd64.tar.gz"
go_archive=${go_url##*/}
go_checksum="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"
go_root="$script_dir/build/golang"

if [[ ! -e "$go_root" ]]; then
  mkdir -p "$script_dir/build"
  if [[ ! -e "$script_dir/build/$go_archive" ]]; then
    echo "downloading golang..."
    wget -q "$go_url" -O "$script_dir/build/$go_archive"
    if [ $? -ne 0 ]; then
      echo "compiler download failed, aborting."
      exit 1
    fi
  fi

  echo "$go_checksum  $script_dir/build/$go_archive" | sha256sum -c -
  if [ $? -ne 0 ]; then
    echo "compiler checksum verification failed, aborting."
    exit 1
  fi

  echo "extracting golang..."
  mkdir -p "$go_root"
  tar --strip-components=1 -xf "$script_dir/build/$go_archive" -C "$go_root"
  if [ $? -ne 0 ]; then
    echo "compiler extraction failed, aborting."
    exit 1
  fi
fi

echo "building $project_name..."
mkdir -p "$script_dir/bin"

go_command="$go_root/bin/go"
export GOTOOLCHAIN=local

"$go_command" -C "$script_dir/src" test ./...
if [ $? -ne 0 ]; then
  echo "tests failed, aborting."
  exit 1
fi

"$go_command" -C "$script_dir/src" vet ./...
if [ $? -ne 0 ]; then
  echo "vet failed, aborting."
  exit 1
fi

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$go_command" -C "$script_dir/src" build -trimpath -ldflags="-s -w" -o "$script_dir/bin/msvcup-linux-amd64" .
if [ $? -ne 0 ]; then
  echo "linux compilation failed, aborting."
  exit 1
fi

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$go_command" -C "$script_dir/src" build -trimpath -ldflags="-s -w" -o "$script_dir/bin/msvcup-windows-amd64.exe" .
if [ $? -ne 0 ]; then
  echo "windows compilation failed, aborting."
  exit 1
fi

echo "done"
