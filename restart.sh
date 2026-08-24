#!/bin/sh
rm -rf "$HOME/.config/brick"
make -C "$(dirname "$0")" build-prod
