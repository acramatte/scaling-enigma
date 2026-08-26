package webapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"
	"semantic-search/internal/storage"
)

const (
	searchLimit    = 20
	searchTimeout  = 15 * time.Second
	maxQueryLength = 2_000
)

type searchFunc func(context.Context, string) ([]appdb.SearchResult, error)

type server struct {
	search      searchFunc
	template    *template.Template
	imageStore  *storage.Store
	imageSecret []byte
}

type pageData struct {
	Query       string
	Searched    bool
	Results     []resultView
	Error       string
	ElapsedTime string
}

type resultView struct {
	Rank             int
	SourceURI        string
	MediaType        string
	CosineSimilarity string
	Segment          string
	ImageURL         string
}

// New returns the HTTP handler for the semantic-search page and can proxy
// s3:// result images when an object store is supplied.
func New(db *appdb.Store, client *embedder.Client, imageStore *storage.Store) http.Handler {
	return newHandler(func(ctx context.Context, query string) ([]appdb.SearchResult, error) {
		embedding, err := client.GetTextEmbeddingContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("embed text query: %w", err)
		}

		return db.SearchDocuments(ctx, appdb.SearchInput{
			Values: embedding.Values,
			Model:  embedding.Model,
			Limit:  searchLimit,
		})
	}, imageStore)
}

func newHandler(search searchFunc, imageStore *storage.Store) http.Handler {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(fmt.Sprintf("generate image token secret: %v", err))
	}
	return &server{
		search:      search,
		template:    template.Must(template.New("search").Parse(pageTemplate)),
		imageStore:  imageStore,
		imageSecret: secret,
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/image" {
		s.serveImage(w, r)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	data := pageData{Query: query}
	if query == "" {
		s.render(w, http.StatusOK, data)
		return
	}
	data.Searched = true

	if utf8.RuneCountInString(query) > maxQueryLength {
		data.Error = "The search query is too long. Please keep it under 2,000 characters."
		s.render(w, http.StatusBadRequest, data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	started := time.Now()
	results, err := s.search(ctx, query)
	data.ElapsedTime = time.Since(started).Round(time.Millisecond).String()
	if err != nil {
		data.Error = "Search is temporarily unavailable. Check that the embedding service and database are running."
		s.render(w, http.StatusInternalServerError, data)
		return
	}

	data.Results = make([]resultView, 0, len(results))
	for index, result := range results {
		data.Results = append(data.Results, resultView{
			Rank:             index + 1,
			SourceURI:        result.SourceURI,
			MediaType:        result.MediaType,
			CosineSimilarity: fmt.Sprintf("%.4f", result.Similarity),
			Segment:          formatSegment(result),
			ImageURL:         s.imageURL(result),
		})
	}

	s.render(w, http.StatusOK, data)
}

func (s *server) render(w http.ResponseWriter, status int, data pageData) {
	var body bytes.Buffer
	if err := s.template.Execute(&body, data); err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = body.WriteTo(w)
}

func (s *server) imageURL(result appdb.SearchResult) string {
	if result.MediaType != appdb.MediaTypeImage || s.imageStore == nil {
		return ""
	}
	parsed, err := url.Parse(result.SourceURI)
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" || strings.TrimPrefix(parsed.EscapedPath(), "/") == "" {
		return ""
	}
	values := url.Values{}
	values.Set("src", result.SourceURI)
	values.Set("sig", s.signImageSource(result.SourceURI))
	return "/image?" + values.Encode()
}

func (s *server) signImageSource(sourceURI string) string {
	mac := hmac.New(sha256.New, s.imageSecret)
	_, _ = mac.Write([]byte(sourceURI))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *server) validImageSignature(sourceURI, signature string) bool {
	expected := s.signImageSource(sourceURI)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) == 1
}

func (s *server) serveImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sourceURI := r.URL.Query().Get("src")
	if sourceURI == "" || !s.validImageSignature(sourceURI, r.URL.Query().Get("sig")) {
		http.NotFound(w, r)
		return
	}
	parsed, err := url.Parse(sourceURI)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch parsed.Scheme {
	case "s3":
		s.serveS3Image(w, r, parsed)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) serveS3Image(w http.ResponseWriter, r *http.Request, parsed *url.URL) {
	if s.imageStore == nil {
		http.NotFound(w, r)
		return
	}
	key, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil || key == "" || parsed.Host == "" {
		http.NotFound(w, r)
		return
	}
	object, err := s.imageStore.Get(r.Context(), parsed.Host, key, parsed.Query().Get("versionId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer object.Body.Close()
	contentType := contentTypeForImage(key, object.ContentType)
	if !strings.HasPrefix(contentType, "image/") {
		http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, object.Body)
}

func contentTypeForImage(name, contentType string) string {
	if before, _, found := strings.CutLast(contentType, ";"); found {
		contentType = before
	}
	contentType = strings.TrimSpace(contentType)
	if contentType != "" && contentType != "application/octet-stream" {
		return contentType
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

func formatSegment(result appdb.SearchResult) string {
	if result.StartMS == nil && result.EndMS == nil {
		return ""
	}

	start := "start"
	if result.StartMS != nil {
		start = formatMilliseconds(*result.StartMS)
	}
	end := "end"
	if result.EndMS != nil {
		end = formatMilliseconds(*result.EndMS)
	}
	return fmt.Sprintf("Segment %d · %s–%s", result.SegmentIndex, start, end)
}

func formatMilliseconds(milliseconds int64) string {
	return (time.Duration(milliseconds) * time.Millisecond).String()
}

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Visual semantic search</title>
  <style>
    :root { color-scheme: light; font-family: Inter, ui-sans-serif, system-ui, sans-serif; color: #18221c; background: #eef2ed; }
    * { box-sizing: border-box; }
    body { margin: 0; }
    main { width: min(1040px, calc(100% - 32px)); margin: 0 auto; padding: 72px 0; }
    header { margin-bottom: 36px; }
    .eyebrow { margin: 0 0 10px; color: #397151; font-size: .78rem; font-weight: 800; letter-spacing: .13em; text-transform: uppercase; }
    h1 { margin: 0; max-width: 680px; font-family: Georgia, serif; font-size: clamp(2.5rem, 7vw, 5rem); font-weight: 500; line-height: .95; letter-spacing: -.045em; }
    .intro { max-width: 590px; margin: 20px 0 0; color: #526058; font-size: 1.05rem; line-height: 1.6; }
    form { padding: 18px; border: 1px solid #d2dbd2; border-radius: 18px; background: #fff; box-shadow: 0 14px 45px rgba(34, 58, 43, .08); }
    label { display: block; margin: 0 0 10px; font-size: .86rem; font-weight: 750; }
    .search-row { display: flex; gap: 10px; }
    input { min-width: 0; flex: 1; border: 1px solid #bdc9bf; border-radius: 11px; padding: 14px 16px; color: inherit; background: #fafcf9; font: inherit; }
    input:focus { outline: 3px solid #c9e4d1; border-color: #397151; }
    button { border: 0; border-radius: 11px; padding: 0 22px; color: #fff; background: #24543a; font: inherit; font-weight: 750; cursor: pointer; }
    button:hover { background: #183e2a; }
    .summary { display: flex; justify-content: space-between; gap: 20px; margin: 38px 2px 14px; color: #526058; font-size: .9rem; }
    .summary strong { color: #18221c; }
    ol { display: grid; gap: 14px; margin: 0; padding: 0; list-style: none; }
    li { display: grid; grid-template-columns: 42px 150px 1fr auto; gap: 16px; align-items: center; padding: 18px; border: 1px solid #d2dbd2; border-radius: 14px; background: rgba(255,255,255,.72); }
    .rank { display: grid; width: 38px; height: 38px; place-items: center; border-radius: 50%; color: #397151; background: #e0ede3; font-weight: 800; }
    .thumbnail { width: 150px; height: 112px; object-fit: cover; border-radius: 11px; border: 1px solid #d2dbd2; background: #e7eee7; }
    .thumbnail-placeholder { display: grid; width: 150px; height: 112px; place-items: center; border: 1px dashed #bdc9bf; border-radius: 11px; color: #68736c; background: #f4f7f3; font-size: .78rem; text-align: center; }
    .uri { overflow-wrap: anywhere; font-family: ui-monospace, SFMono-Regular, Consolas, monospace; font-size: .9rem; }
    .meta { margin-top: 7px; color: #68736c; font-size: .8rem; text-transform: capitalize; }
    .score { color: #24543a; font-weight: 800; white-space: nowrap; }
    .empty, .error { margin-top: 22px; padding: 18px; border-radius: 12px; }
    .empty { border: 1px dashed #bdc9bf; color: #526058; }
    .error { border: 1px solid #e7b8b1; color: #792d25; background: #fff1ee; }
    @media (max-width: 760px) { main { padding-top: 44px; } .search-row { flex-direction: column; } button { padding: 14px; } li { grid-template-columns: 38px 1fr; } .thumbnail, .thumbnail-placeholder { grid-column: 2; width: 100%; max-width: 240px; } .score { grid-column: 2; } }
  </style>
</head>
<body>
  <main>
    <header>
      <p class="eyebrow">Local visual search</p>
      <h1>Find an image by describing it.</h1>
      <p class="intro">Search indexed photos and video segments by meaning, not filenames. Embeddings and results stay on your machine.</p>
    </header>

    <form method="get" action="/">
      <label for="q">What are you looking for?</label>
      <div class="search-row">
        <input id="q" name="q" type="search" value="{{.Query}}" placeholder="A sunset over a mountain ridge" maxlength="2000" autofocus required>
        <button type="submit">Search</button>
      </div>
    </form>

    {{if .Error}}<div class="error" role="alert">{{.Error}}</div>{{end}}
    {{if and .Searched (not .Error)}}
      <div class="summary"><strong>{{len .Results}} results</strong><span>{{.ElapsedTime}}</span></div>
      {{if .Results}}
      <ol>
        {{range .Results}}
        <li>
          <span class="rank">{{.Rank}}</span>
          {{if .ImageURL}}<img class="thumbnail" src="{{.ImageURL}}" alt="Search result {{.Rank}} preview" loading="lazy">{{else}}<div class="thumbnail-placeholder">No preview</div>{{end}}
          <div>
            <div class="uri">{{.SourceURI}}</div>
            <div class="meta">{{.MediaType}}{{if .Segment}} · {{.Segment}}{{end}}</div>
          </div>
          <span class="score">cosine {{.CosineSimilarity}}</span>
        </li>
        {{end}}
      </ol>
      {{else}}
      <div class="empty">No indexed documents matched this query.</div>
      {{end}}
    {{end}}
  </main>
</body>
</html>`
