# hwmond icon

`gen.go` draws the application icon (the tray's meter-bars motif) and writes
`hwmond.ico`, `hwmond.png` and the `../rsrc_windows_{amd64,arm64}.syso`
resource objects that `go build` links into `hwmond.exe` automatically. The
Windows tray loads the embedded group icon (id 1) with `LoadImage`.

Regenerate after changing the drawing:

    cd daemon/icon && go run gen.go

The generator uses only the standard library; the `.syso` is a hand-built
COFF object (one `.rsrc` section, one `ADDR32NB` relocation per resource).
