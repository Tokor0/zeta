package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"
	"zeta/internal/cache"
	"zeta/internal/config"
	"zeta/internal/manager"
	"zeta/internal/parser"
	"zeta/internal/resolver"
	"zeta/internal/scanner"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

func (s *Server) initialize(
	context *glsp.Context,
	params *protocol.InitializeParams,
) (any, error) {
	config, err := config.Load(params.InitializationOptions)
	if err != nil {
		return nil, err
	}

	s.config = config
	log.Printf("Config: %v", config)

	// Record optional client capabilities.
	if w := params.Capabilities.Window; w != nil && w.ShowDocument != nil {
		s.supportsShowDocument = w.ShowDocument.Support
	}

	// Determine the workspace root. `rootUri` is deprecated and may be null
	// (per the LSP spec), so fall back to the first workspace folder and
	// finally to the (also deprecated) rootPath before giving up.
	rootURIString, err := workspaceRoot(params)
	if err != nil {
		return nil, err
	}
	rootUri, err := url.Parse(rootURIString)
	if err != nil {
		return nil, fmt.Errorf("invalid workspace root URI %q: %w", rootURIString, err)
	}
	resolver.Configure(
		rootUri.Path,
		config.SelectRegex,
		config.FileExtensions,
		config.DefaultExtension,
		config.TitleTemplate,
		config.TitleSubstitutions,
		config.DisplayTemplate,
		config.DisplaySubstitutions,
	)

	// Cache File
	stateBaseDir, _ := getXDGStateHome("zeta")
	hash := sha256.New()
	b, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	hash.Write([]byte(b))
	configHash := hex.EncodeToString(hash.Sum(nil))
	cacheDir := path.Join(stateBaseDir, url.PathEscape(rootUri.Path), configHash)
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create state directory: %w", err)
	}
	cacheFile := path.Join(cacheDir, "cache.json")

	// Restore from cache.
	dump, err := os.ReadFile(cacheFile)
	if err != nil {
		s.cache = cache.NewCache()
	} else {
		s.cache, err = cache.RestoreCache(dump)
		if err != nil {
			s.cache = cache.NewCache()
		}
	}

	// Document Manager
	s.manager = manager.NewDocumentManager()

	// Parsers
	s.parsers = parser.NewParserPool(10)

	// Note directory scanning + cache validation.
	seenNotes := map[cache.Path]struct{}{}
	skip := func(absolutepath string, info fs.FileInfo) bool {
		note, err := resolver.Resolve(absolutepath)
		if err != nil {
			return true
		}
		seenNotes[note.CachePath] = struct{}{}
		lastSeen := s.cache.GetSaveTime(note.CachePath)

		hasNotChanged := lastSeen.After(info.ModTime())
		if !hasNotChanged {
			log.Printf("Note %s was was changed", absolutepath)
		}
		return hasNotChanged
	}
	now := time.Now()

	callback := func(absolutepath string, document []byte) {
		note, err := resolver.Resolve(absolutepath)
		if err != nil {
			log.Printf("Unexpected error resolving %v", err)
		}
		nodes, err := s.parsers.ParseAndQuery(document, []byte(s.config.Query))
		if err != nil {
			log.Printf("Unexpected error parsing %v", err)
		}
		links, meta := resolver.ExtractLinksAndMeta(note, nodes, document)
		err = s.cache.SaveNote(note.CachePath, links, meta, now)
		if err != nil {
			log.Println(err)
		}
	}

	go func() {
		scanner.Scan(rootUri.Path, skip, callback)
		notes := s.cache.GetPaths()
		for _, note := range notes {
			if _, ok := seenNotes[note]; !ok {
				s.cache.DeleteNote(note)
			}
		}
	}()

	// Start cache dump routine.
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for range ticker.C {
			log.Printf("Dumping cache to %s", cacheFile)
			dump := s.cache.Dump()
			err := os.WriteFile(cacheFile, dump, 0644)
			if err != nil {
				log.Printf("Error during cache dump: %v", err)
			}
		}
	}()

	syncKind := protocol.TextDocumentSyncKindIncremental

	capabilities := s.handler.CreateServerCapabilities()
	capabilities.TextDocumentSync = &protocol.TextDocumentSyncOptions{
		OpenClose: &protocol.True,
		Change:    &syncKind,
		Save:      &protocol.SaveOptions{IncludeText: &protocol.True},
	}
	// The LSP spec requires `ExecuteCommandOptions.commands` to enumerate the
	// commands the server handles, so clients know which are available.
	capabilities.ExecuteCommandProvider = &protocol.ExecuteCommandOptions{
		Commands: []string{"graph", "createNote"},
	}
	// Offer link-target completion; `"` triggers it inside `#link("…")`.
	capabilities.CompletionProvider = &protocol.CompletionOptions{
		TriggerCharacters: []string{"\""},
	}

	return protocol.InitializeResult{
		Capabilities: capabilities,
	}, nil
}

func (s *Server) initialized(
	context *glsp.Context,
	params *protocol.InitializedParams,
) error {
	log.Println("Client initialized.")
	return nil
}

func (s *Server) shutdown(context *glsp.Context) error {
	return nil
}

// workspaceRoot resolves the workspace root URI from the initialize params.
// `rootUri` is preferred but is deprecated and may be null, so we fall back to
// the first workspace folder and then to the (also deprecated) `rootPath`.
func workspaceRoot(params *protocol.InitializeParams) (string, error) {
	if params.RootURI != nil && *params.RootURI != "" {
		return string(*params.RootURI), nil
	}
	if len(params.WorkspaceFolders) > 0 && params.WorkspaceFolders[0].URI != "" {
		return string(params.WorkspaceFolders[0].URI), nil
	}
	if params.RootPath != nil && *params.RootPath != "" {
		u := url.URL{Scheme: "file", Path: *params.RootPath}
		return u.String(), nil
	}
	return "", fmt.Errorf("no workspace root provided: rootUri, workspaceFolders and rootPath are all empty")
}

func getXDGStateHome(appName string) (string, error) {
	xdgStateHome := os.Getenv("XDG_STATE_HOME")
	if xdgStateHome == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get user home directory: %w", err)
		}
		xdgStateHome = filepath.Join(homeDir, ".local", "state")
	}

	// Final path for your app
	appStateDir := filepath.Join(xdgStateHome, appName)

	// Create it if it doesn't exist
	if err := os.MkdirAll(appStateDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create state directory: %w", err)
	}

	return appStateDir, nil
}
