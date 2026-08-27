// A tiny HTTP server that accepts markdown issue reports and stores them
// in its own KDS (ckdbs) database.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// Must match kds.conf's inline_cell_width. Every varchar column gets
	// exactly this many bytes, tag byte and u16 length included
	// (docs/spec/types.md), and no var-heap exists yet to spill into —
	// 2000 was chosen with margin below the 2026 ceiling `issues`' 4
	// varchar columns impose (see kds.conf's comment).
	cellWidth    = 2000
	cellOverhead = 3
	maxTextBytes = cellWidth - cellOverhead
)

// version/state/category become raw bytes inside a single-quoted KDS
// string literal, and the engine's grammar has no quote-escaping (manual/sql/sql.md
// "no quote escaping in string literals") — so a bare quote in a path
// segment would break out of the literal. Restricting the charset up
// front is what keeps INSERT construction safe without an escaper.
var identRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type insertRequest struct {
	Text string `json:"text"`
}

func main() {
	kdsAddr := envOr("KDS_ADDR", "127.0.0.1:15432")
	httpAddr := envOr("HTTP_ADDR", ":8080")

	db := NewKDSClient(kdsAddr)
	if err := db.Connect(10, 500*time.Millisecond); err != nil {
		log.Fatalf("cannot reach kds at %s (start it first: ckdbs/build-release/kds_server --config kds.conf): %v", kdsAddr, err)
	}
	if err := ensureIssuesTable(db); err != nil {
		log.Fatalf("issues table: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /issues/{version}/{state}/{category}/{$}", handleCreateIssue(db))

	log.Printf("listening on %s, kds at %s", httpAddr, kdsAddr)
	log.Fatal(http.ListenAndServe(httpAddr, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func ensureIssuesTable(db *KDSClient) error {
	reply, err := db.Exec("SHOW TABLES")
	if err != nil {
		return err
	}
	for _, t := range strings.Fields(reply) {
		if t == "issues" {
			return nil
		}
	}
	_, err = db.Exec("CREATE TABLE issues (id int64, version varchar, state varchar, category varchar, body varchar) BTREE")
	return err
}

func handleCreateIssue(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := map[string]string{
			"version":  r.PathValue("version"),
			"state":    r.PathValue("state"),
			"category": r.PathValue("category"),
		}
		for name, v := range path {
			if !identRe.MatchString(v) {
				http.Error(w, fmt.Sprintf("invalid %s %q: must match %s", name, v, identRe.String()), http.StatusBadRequest)
				return
			}
		}

		var req insertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Text == "" {
			http.Error(w, "text must not be empty", http.StatusBadRequest)
			return
		}

		// Stored base64: the wire protocol is one line per command with no
		// raw-newline or quote-escaping (manual/client/client.md §2), and
		// markdown routinely contains both.
		body := base64.StdEncoding.EncodeToString([]byte(req.Text))
		if len(body) > maxTextBytes {
			http.Error(w, fmt.Sprintf("text too large: %d bytes base64-encoded, cell capacity is %d", len(body), maxTextBytes), http.StatusRequestEntityTooLarge)
			return
		}

		stmt := fmt.Sprintf("INSERT INTO issues VALUES ('%s', '%s', '%s', '%s')",
			path["version"], path["state"], path["category"], body)
		reply, err := db.Exec(stmt)
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"version":  path["version"],
			"state":    path["state"],
			"category": path["category"],
			"kds":      reply,
		})
	}
}
