package gameserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

// 一键领取（19/4）逐封隔离失败：附件坏掉的邮件只跳过自己，不能终止后面
// 邮件的准备。否则坏邮件排在前面时，玩家反复点领取也领不出后面的好邮件。

// insertMailWithRawItems 直接写库，构造 MailInsert 拒绝产生的畸形附件数据。
func insertMailWithRawItems(t *testing.T, db *store.DB, uid int64, itemsJSON string, snapshotVersion int64) int64 {
	t.Helper()
	res, err := db.SQL.Exec(`INSERT INTO mail(uid,sender,kind,text,items_json,attachment_snapshot_version,created_ts,read)
		VALUES(?,0,1,'',?,?,0,0)`, uid, itemsJSON, snapshotVersion)
	if err != nil {
		t.Fatalf("写入畸形附件邮件: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取畸形附件邮件编号: %v", err)
	}
	return id
}

// mailClaimBackpack 汇总背包中每个 item_id 的件数。
func mailClaimBackpack(t *testing.T, db *store.DB, uid int64) map[string]int {
	t.Helper()
	rows, err := db.ListItemsByContainer(context.Background(), uid, "backpack")
	if err != nil {
		t.Fatalf("读取背包物品: %v", err)
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.ItemID]++
	}
	return counts
}

// assertMailUnclaimed 断言邮件保持未领取：未读且附件原样保留。
func assertMailUnclaimed(t *testing.T, db *store.DB, uid, id int64) {
	t.Helper()
	mail, err := db.MailGet(context.Background(), uid, id)
	if err != nil {
		t.Fatalf("读取邮件 %d: %v", id, err)
	}
	if mail.Read != 0 || mail.ItemsJSON == "" {
		t.Fatalf("邮件 %d 应保持未领取: read=%d items=%q，期望 read=0 且附件保留", id, mail.Read, mail.ItemsJSON)
	}
}

// assertMailClaimed 断言邮件已置读并清空附件。
func assertMailClaimed(t *testing.T, db *store.DB, uid, id int64) {
	t.Helper()
	mail, err := db.MailGet(context.Background(), uid, id)
	if err != nil {
		t.Fatalf("读取邮件 %d: %v", id, err)
	}
	if mail.Read != 1 || mail.ItemsJSON != "" {
		t.Fatalf("邮件 %d 应已领取: read=%d items=%q，期望 read=1 且附件清空", id, mail.Read, mail.ItemsJSON)
	}
}

// TestMailAcceptAllContinuesAfterCorruptAttachmentJSON：第 2 封附件 JSON 损坏，
// 前后两封正常邮件必须照常入包并置读，坏邮件进 skipped 且保持未读。
func TestMailAcceptAllContinuesAfterCorruptAttachmentJSON(t *testing.T) {
	t.Parallel()
	srv, db := newEconomyTxTestServer(t)
	ctx := context.Background()
	const uid = int64(4311)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatalf("准备角色: %v", err)
	}
	seedEconomyItemDef(t, db, 9431, "mail_skip_first", 28)
	seedEconomyItemDef(t, db, 9432, "mail_skip_third", 28)
	firstID, err := db.MailInsert(ctx, uid, 0, 0, "正常奖励一",
		[]store.MailItem{{ItemID: "mail_skip_first", Qty: 1}})
	if err != nil {
		t.Fatalf("写入第一封正常邮件: %v", err)
	}
	corruptID := insertMailWithRawItems(t, db, uid, `[{"item_id":"mail_skip_corrupt","qty":1}`, 0)
	thirdID, err := db.MailInsert(ctx, uid, 0, 0, "正常奖励三",
		[]store.MailItem{{ItemID: "mail_skip_third", Qty: 1}})
	if err != nil {
		t.Fatalf("写入第三封正常邮件: %v", err)
	}

	count, items, err := srv.acceptMails(economyTestCtx(srv, uid, proto.Args524()),
		[]int64{firstID, corruptID, thirdID})
	if err == nil {
		t.Fatal("坏附件邮件必须回可读错误，实际 err=nil")
	}
	if want := fmt.Sprintf("邮件 %d 附件数据损坏", corruptID); !strings.Contains(err.Error(), want) {
		t.Fatalf("错误未指明坏邮件: %v，期望包含 %q", err, want)
	}
	if count != 2 || len(items) != 2 {
		t.Fatalf("两封正常邮件都应发放: count=%d items=%d，期望 2 和 2", count, len(items))
	}
	got := mailClaimBackpack(t, db, uid)
	if got["mail_skip_first"] != 1 || got["mail_skip_third"] != 1 || len(got) != 2 {
		t.Fatalf("背包物品=%v，期望坏邮件前后两件都入包", got)
	}
	assertMailClaimed(t, db, uid, firstID)
	assertMailClaimed(t, db, uid, thirdID)
	assertMailUnclaimed(t, db, uid, corruptID)
}

// TestMailAcceptAllContinuesAfterIncompleteAttachmentSnapshot：附件快照不完整的
// 邮件同样只跳过自己，后面的正常邮件照常发放。
func TestMailAcceptAllContinuesAfterIncompleteAttachmentSnapshot(t *testing.T) {
	t.Parallel()
	srv, db := newEconomyTxTestServer(t)
	ctx := context.Background()
	const uid = int64(4321)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatalf("准备角色: %v", err)
	}
	seedEconomyItemDef(t, db, 9441, "mail_skip_snap_ok", 28)
	brokenID := insertMailWithRawItems(t, db, uid,
		`[{"item_id":"mail_skip_snap_bad","qty":1}]`, mailItemSnapshotVersion)
	goodID, err := db.MailInsert(ctx, uid, 0, 0, "正常奖励",
		[]store.MailItem{{ItemID: "mail_skip_snap_ok", Qty: 1}})
	if err != nil {
		t.Fatalf("写入正常邮件: %v", err)
	}

	count, items, err := srv.acceptMails(economyTestCtx(srv, uid, proto.Args524()),
		[]int64{brokenID, goodID})
	if err == nil {
		t.Fatal("快照不完整必须回可读错误，实际 err=nil")
	}
	if want := fmt.Sprintf("邮件 %d 附件快照不完整", brokenID); !strings.Contains(err.Error(), want) {
		t.Fatalf("错误未指明坏邮件: %v，期望包含 %q", err, want)
	}
	if count != 1 || len(items) != 1 || items[0].ItemID != "mail_skip_snap_ok" {
		t.Fatalf("坏邮件之后的正常邮件必须发放: count=%d items=%d，期望 1 和 1", count, len(items))
	}
	assertMailClaimed(t, db, uid, goodID)
	assertMailUnclaimed(t, db, uid, brokenID)
}

// TestMailAcceptAllGrantsAllHealthyMails 回归：全部邮件正常时一次领完，
// 不因逐封隔离逻辑漏发或多发。
func TestMailAcceptAllGrantsAllHealthyMails(t *testing.T) {
	t.Parallel()
	srv, db := newEconomyTxTestServer(t)
	ctx := context.Background()
	const uid = int64(4331)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatalf("准备角色: %v", err)
	}
	names := []string{"mail_skip_a", "mail_skip_b", "mail_skip_c"}
	ids := make([]int64, 0, len(names))
	for i, name := range names {
		seedEconomyItemDef(t, db, int64(9451+i), name, 28)
		id, err := db.MailInsert(ctx, uid, 0, 0, name, []store.MailItem{{ItemID: name, Qty: 1}})
		if err != nil {
			t.Fatalf("写入邮件 %s: %v", name, err)
		}
		ids = append(ids, id)
	}

	count, items, err := srv.acceptMails(economyTestCtx(srv, uid, proto.Args524()), ids)
	if err != nil {
		t.Fatalf("正常一键领取不应报错: %v", err)
	}
	if count != len(names) || len(items) != len(names) {
		t.Fatalf("正常一键领取应发放 %d 件: count=%d items=%d", len(names), count, len(items))
	}
	got := mailClaimBackpack(t, db, uid)
	for _, name := range names {
		if got[name] != 1 {
			t.Fatalf("背包缺少 %s: %v", name, got)
		}
	}
	for _, id := range ids {
		assertMailClaimed(t, db, uid, id)
	}
}
