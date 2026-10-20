package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 验证静态资源托管与 /api 兜底逻辑（不依赖数据库）。
func TestStaticServing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "index.html"), []byte("<html>INDEX</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "assets", "a.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "favicon.jpg"), []byte("JPEG"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"code": 0})
	})
	r.Static("/assets", "./web/assets")
	r.StaticFile("/favicon.jpg", "./web/favicon.jpg")
	r.StaticFile("/", "./web/index.html")
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if p == "/api" || strings.HasPrefix(p, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "not found"})
			return
		}
		c.File("./web/index.html")
	})

	okCases := []struct {
		path string
		want string
	}{
		{"/", "INDEX"},
		{"/assets/a.js", "console.log(1)"},
		{"/favicon.jpg", "JPEG"},
		{"/some/unknown/route", "INDEX"},
		{"/apifoo", "INDEX"},
	}
	for _, tc := range okCases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: code=%d body=%q", tc.path, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/not-found", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not found") {
		t.Fatalf("/api/not-found: code=%d body=%q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "\"code\":0") {
		t.Fatalf("/api/health: code=%d body=%q", w.Code, w.Body.String())
	}
}
