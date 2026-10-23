package service

import (
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"xbt2/server/internal/antiban"
	"xbt2/server/internal/model"
)

// TestAccountProfileStoreRoundTrip 验证画像表真实的 upsert 路径。
// 没有数据库时自动跳过；有数据库时用一条假号码跑完就清理。
//
//	XBT_TEST_DSN="host=127.0.0.1 port=5432 user=... dbname=..." go test ./internal/service -run AccountProfile -v
func TestAccountProfileStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("XBT_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 XBT_TEST_DSN，跳过数据库集成测试")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.AccountProfile{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	const mobile = "19900000001"
	t.Cleanup(func() {
		db.Where("mobile = ?", mobile).Delete(&model.AccountProfile{})
	})
	db.Where("mobile = ?", mobile).Delete(&model.AccountProfile{})

	store := NewAccountProfileStore(db)
	if _, ok := store.Load(mobile); ok {
		t.Fatalf("新号码不应有历史画像")
	}

	first := antiban.Profile{
		Mobile:      mobile,
		UserAgent:   "Dalvik/2.1.0 (Linux; U; Android 14; Test) ",
		DeviceCode:  "0123456789abcdef0123456789abcdef",
		DeviceModel: "Test Phone",
		ProxyURL:    "http://user:pass@127.0.0.1:8888",
	}
	if err := store.Save(first); err != nil {
		t.Fatalf("保存画像失败: %v", err)
	}

	// 换一个 store 实例（模拟进程重启）必须能读回同样的指纹。
	reloaded := NewAccountProfileStore(db)
	got, ok := reloaded.Load(mobile)
	if !ok {
		t.Fatalf("画像未落库")
	}
	if got.DeviceCode != first.DeviceCode || got.UserAgent != first.UserAgent || got.ProxyURL != first.ProxyURL {
		t.Fatalf("读回的画像不一致: %+v", got)
	}

	// 再次保存不能产生第二行，且只更新非空字段。
	if err := reloaded.Save(antiban.Profile{Mobile: mobile, ExitIP: "1.2.3.4"}); err != nil {
		t.Fatalf("二次保存失败: %v", err)
	}
	var count int64
	db.Model(&model.AccountProfile{}).Where("mobile = ?", mobile).Count(&count)
	if count != 1 {
		t.Fatalf("upsert 应只保留一行，实际 %d", count)
	}
	got, _ = reloaded.Load(mobile)
	if got.DeviceCode != first.DeviceCode {
		t.Fatalf("空字段覆盖了已有指纹: %+v", got)
	}
	if got.ExitIP != "1.2.3.4" {
		t.Fatalf("exit ip 未更新: %+v", got)
	}

	if err := reloaded.BindUID(mobile, 123456); err != nil {
		t.Fatalf("绑定 uid 失败: %v", err)
	}
	var row model.AccountProfile
	if err := db.Where("mobile = ?", mobile).First(&row).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if row.UID != 123456 {
		t.Fatalf("uid 未写入: %d", row.UID)
	}
}
