#!/bin/sh
# Smoke-test wrapper: vibecli's main.go hardcodes "chat" as argv[1] when
# spawning the inner CLI; ignore that arg and run an interactive bash
# with a predictable prompt for matching in puppeteer tests.
exec env PS1='$ ' /bin/bash --norc --noprofile -i
