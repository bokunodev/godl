all:
	@gobuild -o cmd/godl ./src/*.go

install: all
	@upx --best --lzma -q -f -k -o $(PREFIX)/bin/godl cmd/godl
