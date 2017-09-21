ifeq ($(V),)
    VERBOSE_FLAG=
else
    VERBOSE_FLAG=-v
endif

test:
	go test $(VERBOSE_FLAG) ./...

dist:
	gox \
	    -os='linux' \
	    -arch='386 amd64' \
	    -output='build/{{.Dir}}_{{.OS}}_{{.Arch}}'

release: dist
	git describe --tags --exact-match
	ghr -draft $$(git describe --tags --exact-match) build/

release-pre: dist
	ghr -draft -prerelease pre build/

.PHONY: dist release release-pre
