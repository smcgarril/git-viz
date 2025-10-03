package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

func main() {
	var err error
	db, err = sql.Open("sqlite3", "./gitvis.db")
	if err != nil {
		log.Fatal(err)
	}
	if err := initDB(); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", uploadForm)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/graph/", graphPageHandler) // /graph/{id}  and /graph/{id}/json
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	log.Println("listening :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func initDB() error {
	schema, err := os.ReadFile("db_init.sql")
	if err != nil {
		return err
	}
	_, err = db.Exec(string(schema))
	return err
}

func uploadForm(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "templates/upload.html")
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	f, header, err := r.FormFile("repo")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer f.Close()
	name := header.Filename
	tmp, err := os.CreateTemp("", "repo-*.zip")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, f); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	tmpPath := tmp.Name()
	res, err := db.Exec("INSERT INTO uploads(name) VALUES(?)", name)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	uploadID64, _ := res.LastInsertId()
	uploadID := int(uploadID64)

	extractDir := filepath.Join(os.TempDir(), fmt.Sprintf("gitvis-%d-%d", uploadID, time.Now().UnixNano()))
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := unzipTo(tmpPath, extractDir); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := parseAndStoreRepo(extractDir, uploadID); err != nil {
		http.Error(w, "parse error: "+err.Error(), 500)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/graph/%d", uploadID), http.StatusSeeOther)
}

func unzipTo(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		outPath := filepath.Join(dest, f.Name)
		if f.FileInfo().IsDir() {
			os.MkdirAll(outPath, f.Mode())
			continue
		}
		os.MkdirAll(filepath.Dir(outPath), 0755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(outFile, rc); err != nil {
			rc.Close()
			outFile.Close()
			return err
		}
		outFile.Close()
		rc.Close()
	}
	return nil
}

func parseAndStoreRepo(root string, uploadID int) error {
	var repoPath string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// detect .git dir
		if info.IsDir() && info.Name() == ".git" {
			repoPath = p
			return filepath.SkipDir
		}
		// detect bare repo by presence of HEAD file
		if !info.IsDir() && info.Name() == "HEAD" && repoPath == "" {
			repoPath = filepath.Dir(p)
		}
		return nil
	})
	if repoPath == "" {
		// try root
		repoPath = root
	}

	r, err := git.PlainOpen(repoPath)
	if err != nil {
		// try DetectDotGit
		r, err = git.PlainOpenWithOptions(repoPath, &git.PlainOpenOptions{DetectDotGit: true})
		if err != nil {
			return err
		}
	}

	refs, err := r.References()
	if err != nil {
		return err
	}
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		// consider branches and tags
		if !(ref.Name().IsBranch() || ref.Name().IsTag()) {
			return nil
		}
		cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
		if err != nil {
			return nil
		}
		_ = cIter.ForEach(func(c *object.Commit) error {
			// store commit node
			meta := map[string]interface{}{
				"author": c.Author.Name, "email": c.Author.Email, "time": c.Author.When.String(),
			}
			storeNode(c.Hash.String(), uploadID, "commit", strings.TrimSpace(c.Message), meta)
			// parents
			for _, p := range c.ParentHashes {
				storeNodeIfMissing(p.String(), uploadID, "commit", "")
				storeEdge(uploadID, c.Hash.String(), p.String(), "parent")
			}
			// commit->tree
			tree, err := c.Tree()
			if err == nil {
				storeNodeIfMissing(tree.Hash.String(), uploadID, "tree", "/")
				storeEdge(uploadID, c.Hash.String(), tree.Hash.String(), "commit->tree")
				traverseTree(r, tree, uploadID)
			}
			return nil
		})
		return nil
	})
	return err
}

func traverseTree(r *git.Repository, t *object.Tree, uploadID int) {
	for _, e := range t.Entries {
		if e.Mode.IsFile() {
			// store blob with filename in the label
			storeNode(e.Hash.String(), uploadID, "blob", e.Name, nil)
			storeEdge(uploadID, t.Hash.String(), e.Hash.String(), "tree->blob")
		} else if e.Mode == filemode.Dir {
			// try to load subtree by path
			subtree, err := r.TreeObject(e.Hash)
			if err == nil && subtree != nil {
				storeNodeIfMissing(subtree.Hash.String(), uploadID, "tree", e.Name)
				storeEdge(uploadID, t.Hash.String(), subtree.Hash.String(), "tree->tree")
				traverseTree(r, subtree, uploadID)
			}
		}
	}
}

func storeNode(id string, uploadID int, typ, label string, meta interface{}) {
	metaStr := ""
	if meta != nil {
		b, _ := json.Marshal(meta)
		metaStr = string(b)
	}
	_, _ = db.Exec(`INSERT OR REPLACE INTO nodes(id, upload_id, type, label, meta) VALUES(?,?,?,?,?)`,
		id, uploadID, typ, label, metaStr)
}

func storeNodeIfMissing(id string, uploadID int, typ, label string) {
	_, _ = db.Exec(`INSERT OR IGNORE INTO nodes(id, upload_id, type, label, meta) VALUES(?,?,?,?,?)`,
		id, uploadID, typ, label, "")
}

func storeEdge(uploadID int, source, target, rel string) {
	_, _ = db.Exec(`INSERT INTO edges(upload_id, source, target, rel) VALUES(?,?,?,?)`,
		uploadID, source, target, rel)
}

func graphPageHandler(w http.ResponseWriter, r *http.Request) {
	// expecting /graph/{id} or /graph/{id}/json
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	idStr := parts[1]
	if idStr == "" {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 3 && parts[2] == "json" {
		graphJSONHandler(w, r, idStr)
		return
	}

	// query the upload name
	var uploadName string
	err := db.QueryRow(`SELECT name FROM uploads WHERE id = ?`, idStr).Scan(&uploadName)
	if err != nil {
		uploadName = "(unknown)"
	}

	// render graph.html as a template, injecting RepoID
	t, err := template.ParseFiles("templates/graph.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// inject RepoID and Name into the template
	t.Execute(w, map[string]string{
		"RepoID": idStr,
		"Name":   uploadName,
	})
}

func graphJSONHandler(w http.ResponseWriter, r *http.Request, idStr string) {
	uploadID, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}

	type Node struct {
		ID    string                 `json:"id"`
		Type  string                 `json:"type"`
		Label string                 `json:"label,omitempty"`
		Extra map[string]interface{} `json:"extra,omitempty"`
	}
	type Link struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Rel    string `json:"rel,omitempty"`
	}

	// fetch nodes
	rows, err := db.Query("SELECT id,type,label,meta FROM nodes WHERE upload_id=?", uploadID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	nodes := make([]Node, 0)
	for rows.Next() {
		var id, typ, label, metaStr string
		rows.Scan(&id, &typ, &label, &metaStr)

		var meta map[string]interface{}
		if metaStr != "" {
			_ = json.Unmarshal([]byte(metaStr), &meta)
		}

		// Enhance node info
		extra := make(map[string]interface{})
		if typ == "commit" {
			extra["message"] = meta["message"]
			extra["author"] = meta["author"]
			extra["email"] = meta["email"]
			extra["date"] = meta["time"]
			if label == "" {
				label = id[:7]
			}
		} else if typ == "blob" {
			extra["filename"] = label
			if label == "" {
				label = id[:7]
			}
		} else if typ == "tree" {
			if label == "" {
				label = id[:7]
			}
		}

		nodes = append(nodes, Node{
			ID:    id,
			Type:  typ,
			Label: label,
			Extra: extra,
		})
	}

	// fetch edges
	linkRows, err := db.Query("SELECT source,target,rel FROM edges WHERE upload_id=?", uploadID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer linkRows.Close()

	links := make([]Link, 0)
	for linkRows.Next() {
		var s, t, rel string
		linkRows.Scan(&s, &t, &rel)
		links = append(links, Link{Source: s, Target: t, Rel: rel})
	}

	out := map[string]interface{}{"nodes": nodes, "links": links}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
