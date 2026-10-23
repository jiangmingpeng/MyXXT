package main

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"xbt2/server/internal/antiban"
	"xbt2/server/internal/config"
	"xbt2/server/internal/db"
	"xbt2/server/internal/handler"
	"xbt2/server/internal/middleware"
	"xbt2/server/internal/service"
	"xbt2/server/internal/xxt"
)

func main() {
	cfg := config.Load()
	gin.SetMode(resolveGinMode(cfg.AppEnv))

	database, err := db.New(cfg)
	if err != nil {
		log.Fatalf("db init failed: %v", err)
	}

	jwtSvc := service.NewJWTService(cfg.JWTSecret)
	credentialCrypto := service.NewCredentialCrypto(cfg.CredentialSecret)

	// 反风控引擎：设备指纹 + 出口代理 + 节流 + 熔断。
	// 画像落库，保证重启/重新登录后仍是"同一台手机"。
	profileStore := service.NewAccountProfileStore(database)
	guard := antiban.New(cfg.Antiban.ToAntibanOptions(cfg.ChaoxingUserAgent), profileStore)

	xxtClient := xxt.NewWithOptions(xxt.Options{
		AESKey:            cfg.ChaoxingAESKey,
		FallbackUserAgent: cfg.ChaoxingUserAgent,
		InsecureTLS:       cfg.AllowInsecureTLS,
		ActiveFetchMax:    cfg.ActivityListLimit + 1,
		Guard:             guard,
		UnifyLoginUA:      cfg.Antiban.IsUnifyLoginUA(),
		ClientIPMode:      cfg.Antiban.ClientIPMode,
		FixedClientIP:     cfg.Antiban.ClientIP,
		ExitIPProbeURL:    cfg.Antiban.ExitIPProbeURL,
		LogPayloads:       cfg.Antiban.LogPayloads,
	})

	authHandler := handler.NewAuthHandler(database, jwtSvc, credentialCrypto, xxtClient)
	courseHandler := handler.NewCourseHandler(database, xxtClient, credentialCrypto)
	signSvc := service.NewSignService(database, xxtClient, credentialCrypto, guard)
	signHandler := handler.NewSignHandler(database, xxtClient, credentialCrypto, signSvc, cfg.ActivityListLimit)
	whitelistHandler := handler.NewWhitelistHandler(database)

	logAntiBanSummary(cfg, guard)

	r := gin.Default()

	api := r.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) {
			c.JSON(200, gin.H{"code": 0, "message": "ok", "data": gin.H{"service": "xbt2-server"}})
		})
		api.POST("/auth/login", authHandler.Login)

		authed := api.Group("")
		authed.Use(middleware.Auth(jwtSvc))
		{
			authed.GET("/courses", courseHandler.List)
			authed.POST("/courses/sync", courseHandler.Sync)
			authed.PUT("/courses/selection", courseHandler.UpdateSelection)

			authed.GET("/sign/activities", signHandler.Activities)
			authed.GET("/sign/classmates", signHandler.Classmates)
			authed.POST("/sign/check", signHandler.Check)
			authed.POST("/sign/execute", signHandler.Execute)
			authed.POST("/sign/photo", signHandler.Photo)

			admin := authed.Group("/admin")
			admin.Use(middleware.AdminOnly())
			{
				admin.GET("/whitelist/users", whitelistHandler.ListUsers)
				admin.POST("/whitelist/users", whitelistHandler.CreateUser)
				admin.POST("/whitelist/users/import", whitelistHandler.BatchImportUsers)
				admin.DELETE("/whitelist/users/:id", whitelistHandler.DeleteUser)
			}
		}
	}

	// 前端静态资源：把 Web 的构建产物（Web/dist）放到可执行文件同级的 web/ 目录下。
	// 目录结构示例：
	//   ./xbt-server
	//   ./config.yaml
	//   ./web/index.html
	//   ./web/assets/...
	r.Static("/assets", "./web/assets")
	r.StaticFile("/favicon.jpg", "./web/favicon.jpg")
	r.StaticFile("/", "./web/index.html")

	// 前端使用 HashRouter，浏览器实际只会请求 "/" 和 "/assets/*"。
	// 这里兜底：非 /api 的路径回退到 index.html，避免误访问时返回 404。
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if p == "/api" || strings.HasPrefix(p, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "not found"})
			return
		}
		c.File("./web/index.html")
	})

	log.Printf("xbt2 server listening on %s (app_env=%s, gin_mode=%s)", cfg.HTTPAddr, cfg.AppEnv, gin.Mode())
	if err := r.Run(cfg.HTTPAddr); err != nil {
		log.Fatalf("server start failed: %v", err)
	}
}

func resolveGinMode(appEnv string) string {
	switch strings.ToLower(strings.TrimSpace(appEnv)) {
	case "prod", "production":
		return gin.ReleaseMode
	case "test", "testing":
		return gin.TestMode
	case "dev", "development":
		fallthrough
	default:
		return gin.DebugMode
	}
}

// logAntiBanSummary 在启动时打印一份"当前暴露面"摘要（不含任何密钥/代理账号）。
// 目的是让部署者一眼看出风险等级：没有代理、并发过高、关闭熔断都会被点名。
func logAntiBanSummary(cfg config.Config, guard *antiban.Engine) {
	opts := guard.Options()
	log.Printf("[antiban] enabled=%t proxies=%d max_concurrency=%d write_interval=%s~%s read_interval=%s~%s captcha_attempts=%d breaker=%.1f/%s fingerprints=%d clientip=%s unify_login_ua=%t",
		opts.Enabled, len(opts.ProxyPool), opts.MaxConcurrency,
		opts.MinInterval, opts.MaxInterval, opts.ReadMinInterval, opts.ReadMaxInterval,
		opts.CaptchaMaxAttempts,
		opts.BreakerThreshold, opts.BreakerCooldown, len(opts.DeviceTemplates),
		cfg.Antiban.ClientIPMode, cfg.Antiban.IsUnifyLoginUA())

	if len(opts.ProxyPool) == 0 {
		log.Printf("[antiban] 警告：未配置代理，所有账号共用本机出口 IP —— 这是最容易被判定为批量操作的特征")
	} else if len(opts.ProxyPool) < 3 {
		log.Printf("[antiban] 提示：代理池只有 %d 个出口，账号较多时同 IP 并发仍然偏高", len(opts.ProxyPool))
	}
	if !opts.Enabled {
		log.Printf("[antiban] 警告：反风控已关闭（antiban.enabled=false），节流与熔断不会生效")
	}
	if cfg.AllowInsecureTLS {
		log.Printf("[antiban] 警告：allow_insecure_tls=true 会关闭证书校验，建议改为 false")
	}
}
