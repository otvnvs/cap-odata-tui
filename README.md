# CAP OData TUI

![screenshot](img/preview.png)

## Building

```bash
go build
```

Or

```bash
GOOS=windows GOARCH=amd64 go build -o cap_browser.exe
```

## Usage

```bash
./cap_browser --version
```

```bash
./cap_browser --help
```

```bash
./cap_browser
```

```bash
./cap_browser -url http://localhost:4005
```

The `-url` flag value can be either a CAP service document url or a `$metadata` url

## Keys - Main Menu

Use the arrow keys to navigate, hit Enter to make a selection. Use `b` to exit program.

## Keys - Tables

- `r` - refresh data
- `k` - move up a row
- `j` - move down a row
- `h` - move left a column
- `l` - move right a column
- `n` - go to next page
- `p` - go to previous page
- `enter` - start editing cell (`esc` to back out, `enter` to `PATCH` data
- 'i' - insert new row
- 'x' - delete row
- 'r' - toggle auto refresh
- `b` - back to main screen
