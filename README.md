# gitvis

A minimal Go app that accepts an uploaded zipped `.git` directory (or bare repository tar/zip), parses the objects using `go-git`, stores a compact graph in SQLite, and serves a D3 visualization.

## Quick start

1. Install Go (1.20+).
2. Unzip the repo and run:

```bash
go mod download
go run main.go
```

3. Open http://localhost:8080 and upload a zipped `.git` directory (or a bare repo zip).

**Note:** This is a minimal demo for learning purposes. Do not run this server in production without additional security hardening (sandbox extraction, size limits, auth).

