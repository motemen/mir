ifeq ($(V),)
    VERBOSE_FLAG=
else
    VERBOSE_FLAG=-v
endif

VERSION=$$(git describe --tags --exact-match)

test:
	go test $(VERBOSE_FLAG) ./...

dist:
	rm -fr dist/
	script/build_dist

release: dist
	git describe --tags --exact-match
	ghr -username motemen -draft $(VERSION) dist/$(VERSION)

release-pre: dist
	ghr -usename motemen -draft -prerelease pre dist/

.PHONY: dist release release-pre
