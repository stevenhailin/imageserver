package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/golang/groupcache"
	"github.com/pierrre/githubhook"
	"github.com/pierrre/imageserver"
	imageserver_cache "github.com/pierrre/imageserver/cache"
	imageserver_cache_groupcache "github.com/pierrre/imageserver/cache/groupcache"
	imageserver_cache_memory "github.com/pierrre/imageserver/cache/memory"
	imageserver_http "github.com/pierrre/imageserver/http"
	imageserver_http_nfntresize "github.com/pierrre/imageserver/http/nfntresize"
	imageserver_image "github.com/pierrre/imageserver/image"
	_ "github.com/pierrre/imageserver/image/bmp"
	imageserver_image_gamma "github.com/pierrre/imageserver/image/gamma"
	_ "github.com/pierrre/imageserver/image/gif"
	_ "github.com/pierrre/imageserver/image/jpeg"
	imageserver_image_nfntresize "github.com/pierrre/imageserver/image/nfntresize"
	_ "github.com/pierrre/imageserver/image/png"
	_ "github.com/pierrre/imageserver/image/tiff"
	imageserver_testdata "github.com/pierrre/imageserver/testdata"
)

const (
	groupcacheName = "imageserver"
)

var (
	flagHTTPAddr            = ":8080"
	flagGitHubWebhookSecret string
	flagHTTPExpires         = time.Duration(7 * 24 * time.Hour)
	flagGroupcache          = int64(512 * (1 << 20))
	flagGroupcacheSelf      string
	flagGroupcachePeers     string
	flagCacheMemory         = int64(64 * (1 << 20))
)

func main() {
	parseFlags()
	log.Println("Start")
	logEnv()
	startGroupcacheHTTPServer()
	startHTTPServer()
}

func parseFlags() {
	flag.StringVar(&flagHTTPAddr, "http", flagHTTPAddr, "HTTP addr")
	flag.DurationVar(&flagHTTPExpires, "http-expires", flagHTTPExpires, "HTTP expires")
	flag.StringVar(&flagGitHubWebhookSecret, "github-webhook-secret", flagGitHubWebhookSecret, "GitHub webhook secret")
	flag.Int64Var(&flagGroupcache, "groupcache", flagGroupcache, "Groupcache")
	flag.StringVar(&flagGroupcacheSelf, "groupcache-self", flagGroupcacheSelf, "Groupcache self")
	flag.StringVar(&flagGroupcachePeers, "groupcache-peers", flagGroupcachePeers, "Groupcache peers")
	flag.Int64Var(&flagCacheMemory, "cache-memory", flagCacheMemory, "Cache memory")
	flag.Parse()
}

func logEnv() {
	log.Printf("Go version: %s", runtime.Version())
	log.Printf("Go max procs: %d", runtime.GOMAXPROCS(0))
}

func startHTTPServer() {
	http.Handle("/", newImageHTTPHandler())
	http.Handle("/favicon.ico", http.NotFoundHandler())
	if h := newGitHubWebhookHTTPHandler(); h != nil {
		http.Handle("/github_webhook", h)
	}
	log.Printf("Start HTTP server on %s", flagHTTPAddr)
	err := http.ListenAndServe(flagHTTPAddr, nil)
	if err != nil {
		log.Panic(err)
	}
}

func newGitHubWebhookHTTPHandler() http.Handler {
	if flagGitHubWebhookSecret == "" {
		return nil
	}
	return &githubhook.Handler{
		Secret: flagGitHubWebhookSecret,
		Delivery: func(event string, deliveryID string, payload interface{}) {
			log.Printf("Received GitHub webhook: %s", event)
			if event == "push" {
				delay := time.Duration(5 * time.Second)
				log.Printf("Killing process in %s", delay)
				time.AfterFunc(delay, func() {
					log.Println("Killing process now")
					os.Exit(0)
				})
			}
		},
		Error: func(err error, req *http.Request) {
			log.Printf("GitHub webhook error: %s", err)
		},
	}
}

func newImageHTTPHandler() http.Handler {
	var handler http.Handler
	handler = &imageserver_http.Handler{
		Parser: &imageserver_http.ListParser{
			&imageserver_http.SourceParser{},
			&imageserver_http_nfntresize.Parser{},
			&imageserver_http.FormatParser{},
			&imageserver_http.QualityParser{},
			&imageserver_http.GammaCorrectionParser{},
		},
		Server:   newServer(),
		ETagFunc: imageserver_http.NewParamsHashETagFunc(sha256.New),
		ErrorFunc: func(err error, req *http.Request) {
			log.Printf("Internal error: %s", err)
		},
	}
	if flagHTTPExpires != 0 {
		handler = &imageserver_http.ExpiresHandler{
			Handler: handler,
			Expires: flagHTTPExpires,
		}
	}
	return handler
}

func newServer() imageserver.Server {
	srv := imageserver_testdata.Server
	srv = newServerImage(srv)
	srv = newServerLimit(srv)
	srv = newServerValidate(srv)
	srv = newServerGroupcache(srv)
	srv = newServerCacheMemory(srv)
	return srv
}

func newServerImage(srv imageserver.Server) imageserver.Server {
	return &imageserver_image.Server{
		Server: srv,
		Processor: imageserver_image_gamma.NewCorrectionProcessor(
			&imageserver_image_nfntresize.Processor{},
			true,
		),
	}
}

func newServerLimit(srv imageserver.Server) imageserver.Server {
	return imageserver.NewLimitServer(srv, runtime.GOMAXPROCS(0)*2)
}

func newServerValidate(srv imageserver.Server) imageserver.Server {
	return &imageserver_image_nfntresize.ValidateParamsServer{
		Server:    srv,
		WidthMax:  2048,
		HeightMax: 2048,
	}
}

func newServerGroupcache(srv imageserver.Server) imageserver.Server {
	if flagGroupcache <= 0 {
		return srv
	}
	return imageserver_cache_groupcache.NewServer(
		srv,
		imageserver_cache.NewParamsHashKeyGenerator(sha256.New),
		groupcacheName,
		flagGroupcache,
	)
}

func newServerCacheMemory(srv imageserver.Server) imageserver.Server {
	if flagCacheMemory <= 0 {
		return srv
	}
	return &imageserver_cache.Server{
		Server:       srv,
		Cache:        imageserver_cache_memory.New(flagCacheMemory),
		KeyGenerator: imageserver_cache.NewParamsHashKeyGenerator(sha256.New),
	}
}

func startGroupcacheHTTPServer() {
	if flagGroupcacheSelf == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/", newGroupcacheHTTPPool())
	mux.HandleFunc("/stats", groupcacheStatsHTTPHandler)
	mux.Handle("/favicon.ico", http.NotFoundHandler())
	go func() {
		log.Printf("Start groupcache HTTP server on %s", flagGroupcacheSelf)
		err := http.ListenAndServe(flagGroupcacheSelf, mux)
		if err != nil {
			log.Fatal(err)
		}
	}()
}

func newGroupcacheHTTPPool() *groupcache.HTTPPool {
	self := (&url.URL{Scheme: "http", Host: flagGroupcacheSelf}).String()
	var peers []string
	peers = append(peers, self)
	for _, p := range strings.Split(flagGroupcachePeers, ",") {
		if p == "" {
			continue
		}
		peer := (&url.URL{Scheme: "http", Host: p}).String()
		peers = append(peers, peer)
	}
	opts := &groupcache.HTTPPoolOptions{BasePath: "/"}
	pool := groupcache.NewHTTPPoolOpts(self, opts)
	pool.Context = imageserver_cache_groupcache.HTTPPoolContext
	pool.Transport = imageserver_cache_groupcache.NewHTTPPoolTransport(nil)
	pool.Set(peers...)
	return pool
}

func groupcacheStatsHTTPHandler(w http.ResponseWriter, req *http.Request) {
	gp := groupcache.GetGroup(groupcacheName)
	if gp == nil {
		http.Error(w, fmt.Sprintf("group %s not found", groupcacheName), http.StatusServiceUnavailable)
		return
	}
	type cachesStats struct {
		Main groupcache.CacheStats
		Hot  groupcache.CacheStats
	}
	type stats struct {
		Group  groupcache.Stats
		Caches cachesStats
	}
	data, err := json.MarshalIndent(
		stats{
			Group: gp.Stats,
			Caches: cachesStats{
				Main: gp.CacheStats(groupcache.MainCache),
				Hot:  gp.CacheStats(groupcache.HotCache),
			},
		},
		"",
		"	",
	)
	if err != nil {
		log.Println(err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	_, err = w.Write(data)
	if err != nil {
		log.Println(err)
		return
	}
}
