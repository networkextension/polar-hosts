// Command hosts-svc is the polar-agent host management plugin
// binary. Phase 2-W3 skeleton.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"

	"github.com/networkextension/polar-hosts/internal/hosts"
)

func main() {
	cfg := hosts.Config{
		DBDSN:                    envOrDefault("POLAR_HOSTS_DB_DSN", "postgres://ideamesh:test123456@127.0.0.1:5432/polar_hosts?sslmode=disable"),
		DockBase:                 envOrDefault("POLAR_DOCK_BASE", "http://127.0.0.1:8080"),
		PluginName:               envOrDefault("POLAR_PLUGIN_NAME", "hosts"),
		PluginToken:              os.Getenv("POLAR_PLUGIN_TOKEN"),
		Listen:                   envOrDefault("POLAR_HOSTS_LISTEN", "127.0.0.1:8095"),
		BuildVersion:             envOrDefault("POLAR_HOSTS_VERSION", "0.0.1"),
		BlobDir:                  envOrDefault("POLAR_HOSTS_BLOB_DIR", "/Users/local/hosts-svc-data"),
		MetricsToken:             os.Getenv("POLAR_HOSTS_METRICS_TOKEN"),
		SkillKeyHex:              os.Getenv("HOSTS_SKILL_KEY"),
		PolarAllowLocalBootstrap: envBool("POLAR_ALLOW_LOCAL_BOOTSTRAP", false),
		PolarDisableShellSkill:   envBool("POLAR_DISABLE_SHELL_SKILL", false),
		PolarDisableVNCSkill:     envBool("POLAR_DISABLE_VNC_SKILL", false),
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		log.Fatal("POLAR_PLUGIN_TOKEN unset — get plaintext from /admin-plugins.html")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plugin, err := hosts.New(ctx, cfg)
	if err != nil {
		log.Fatalf("hosts.New: %v", err)
	}
	defer plugin.Close()

	gin.SetMode(envOrDefault("GIN_MODE", gin.ReleaseMode))
	r := gin.New()
	r.Use(gin.Recovery())
	plugin.RegisterRoutes(r)
	plugin.Start(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("hosts-svc listening on %s (dock=%s, name=%s, ver=%s, blob=%s)",
			cfg.Listen, cfg.DockBase, cfg.PluginName, cfg.BuildVersion, cfg.BlobDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("hosts-svc: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("hosts-svc: shutdown: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
