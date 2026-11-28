package gameserver

import (
	"context"
	"testing"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 本文件锁定 PR5：非事务版扣料曾把写库错误吞成「扣成功了」。
// 写库失败时必须中止上游动作：不返回成功份数、不推送、不记窝账。

// enableReadOnlyDB 让之后的写库语句全部失败，模拟 SQLITE_BUSY/连接关闭。
// query_only 是 SQLite 自带的只读开关，读路径不受影响，正好隔离出写失败。
func enableReadOnlyDB(t *testing.T, db *store.DB) {
	t.Helper()
	db.SQL.SetMaxOpenConns(1)
	if _, err := db.SQL.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatalf("打开只读开关失败: %v", err)
	}
	// 自证只读开关真的拦住了写：否则用例会因为写成功而假通过。
	if _, err := db.SQL.Exec(`UPDATE item SET qty=qty WHERE 1=0`); err == nil {
		t.Fatal("只读开关未生效，扣料写库失败路径没有被覆盖")
	}
}

// disableReadOnlyDB 关掉只读开关，供失败用例回头查库存。
func disableReadOnlyDB(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.SQL.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatalf("关闭只读开关失败: %v", err)
	}
}

// itemStackQty 读回实例的剩余件数；实例已删除时返回 -1。
func itemStackQty(t *testing.T, srv *Server, guid string) int64 {
	t.Helper()
	row, err := srv.DB.GetItemByGUID(context.Background(), guid)
	if err != nil || row == nil {
		return -1
	}
	return srv.stackCountOf(context.Background(), row)
}

// TestConsumeItemQtyErrReportsWriteFailure 扣料写库失败必须返回 0 + 错误，
// 且不能把内存行改成扣过的样子：调用方据此中止动作，库存原样保留。
func TestConsumeItemQtyErrReportsWriteFailure(t *testing.T) {
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid = int64(1000001)
	stack, err := srv.CreateItem(ctx, uid, "feedcompleted", "backpack", 1)
	if err != nil {
		t.Fatalf("创建窝料堆失败: %v", err)
	}
	srv.setStackCount(ctx, stack, 3)
	fresh, err := db.GetItemByGUID(ctx, stack.GUID)
	if err != nil || fresh == nil {
		t.Fatalf("读回窝料堆失败: %v", err)
	}

	enableReadOnlyDB(t, db)
	took, cerr := srv.consumeItemQtyErr(ctx, fresh, 1)
	if took != 0 || cerr == nil {
		t.Fatalf("写库失败时 consumeItemQtyErr = %d, err=%v，期望 0 且带回错误", took, cerr)
	}
	if fresh.Qty != 3 {
		t.Fatalf("写库失败后内存行 qty = %d，期望保持 3（不能当成已扣除）", fresh.Qty)
	}

	disableReadOnlyDB(t, db)
	if got := itemStackQty(t, srv, stack.GUID); got != 3 {
		t.Fatalf("写库失败后库存 = %d，期望仍是 3", got)
	}
}

// TestConsumeItemQtyErrSyncsStackFieldMirror 正常路径回归：扣 1 份后 item.qty 与
// 数量字段 ljnnpokapig（csQty）同步下降，物品行与槽位/钓组关联都保留。
func TestConsumeItemQtyErrSyncsStackFieldMirror(t *testing.T) {
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid = int64(1000001)
	stack, err := srv.CreateItem(ctx, uid, "feedcompleted", "backpack", 1)
	if err != nil {
		t.Fatalf("创建窝料堆失败: %v", err)
	}
	srv.setStackCount(ctx, stack, 3)
	if _, err := db.SQL.Exec(`INSERT INTO container_slot(uid,container,slot,guid) VALUES(?,0,7,?)`,
		uid, stack.GUID); err != nil {
		t.Fatalf("写入槽位行失败: %v", err)
	}
	fresh, err := db.GetItemByGUID(ctx, stack.GUID)
	if err != nil || fresh == nil {
		t.Fatalf("读回窝料堆失败: %v", err)
	}
	took, cerr := srv.consumeItemQtyErr(ctx, fresh, 1)
	if cerr != nil || took != 1 {
		t.Fatalf("扣 1 份应成功，实得 %d（err=%v）", took, cerr)
	}
	after, err := db.GetItemByGUID(ctx, stack.GUID)
	if err != nil || after == nil {
		t.Fatalf("扣减后应保留物品行: %v", err)
	}
	if after.Qty != 2 {
		t.Fatalf("扣 1 份后 item.qty = %d，期望 2", after.Qty)
	}
	fields, ferr := FieldsFromJSON(after.FieldsJSON)
	if ferr != nil {
		t.Fatalf("解析扣减后字段失败: %v", ferr)
	}
	if n, ok := itemStackQtyValue(after.WireClass, fields); !ok || n != 2 {
		t.Fatalf("扣 1 份后 ljnnpokapig = %d (ok=%v)，期望 2（字段必须与 item.qty 同步）", n, ok)
	}
	var slots int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM container_slot WHERE guid=?`, stack.GUID).Scan(&slots); err != nil {
		t.Fatalf("统计槽位行失败: %v", err)
	}
	if slots != 1 {
		t.Fatalf("未用尽时槽位行应保留，实得 %d 行", slots)
	}
}

// TestConsumeItemQtyErrDeletesExhaustedRow 正常路径回归：份数用尽时删除整件，
// item / container_slot / rig_component 三张表都不留残行。
func TestConsumeItemQtyErrDeletesExhaustedRow(t *testing.T) {
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid = int64(1000001)
	stack, err := srv.CreateItem(ctx, uid, "feedcompleted", "backpack", 1)
	if err != nil {
		t.Fatalf("创建窝料堆失败: %v", err)
	}
	srv.setStackCount(ctx, stack, 1)
	if _, err := db.SQL.Exec(`INSERT INTO container_slot(uid,container,slot,guid) VALUES(?,0,7,?)`,
		uid, stack.GUID); err != nil {
		t.Fatalf("写入槽位行失败: %v", err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO rig_component(rig_guid,slot,ordinal,guid,extra) VALUES(?,?,0,?,0)`,
		"rig-placeholder", 3, stack.GUID); err != nil {
		t.Fatalf("写入钓组关联行失败: %v", err)
	}
	fresh, err := db.GetItemByGUID(ctx, stack.GUID)
	if err != nil || fresh == nil {
		t.Fatalf("读回窝料堆失败: %v", err)
	}
	took, cerr := srv.consumeItemQtyErr(ctx, fresh, 1)
	if cerr != nil || took != 1 {
		t.Fatalf("用尽一堆应扣 1 份，实得 %d（err=%v）", took, cerr)
	}
	if qty := itemStackQty(t, srv, stack.GUID); qty != -1 {
		t.Fatalf("用尽后物品行应删除，实得剩余 %d", qty)
	}
	for _, table := range []string{"container_slot", "rig_component"} {
		var n int
		if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE guid=?`, stack.GUID).Scan(&n); err != nil {
			t.Fatalf("统计 %s 残行失败: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s 应同步删除，实得 %d 行残留", table, n)
		}
	}
}

// TestDynamicHanditemThrowAbortsWhenConsumeFails 13/5 弹弓扣料写库失败时这一发不生效：
// 装填物份数不变、手持物仍指向原弹药、窝账不新增。
func TestDynamicHanditemThrowAbortsWhenConsumeFails(t *testing.T) {
	srv, db := newRigTestServer(t)
	registerDynamicProtocolHandlers(srv.dispatch)
	setWeatherTestTunable(t, srv, "groundbait_enabled", "1")
	ctx := context.Background()
	const uid = int64(1000001)
	st := srv.stateFor(uid)
	st.mu.Lock()
	st.Loc = "gb-test-loc"
	st.mu.Unlock()
	hand, err := srv.CreateItem(ctx, uid, "wh_pc_gb", "backpack", 1)
	if err != nil {
		t.Fatalf("创建手持物失败: %v", err)
	}
	bait, err := srv.CreateItem(ctx, uid, "worm_01", "rig", 1)
	if err != nil {
		t.Fatalf("创建装填物失败: %v", err)
	}
	srv.setStackCount(ctx, bait, 2)
	srv.setHanditemLoadedRef(ctx, hand.GUID, bait.GUID)
	req := rigRequestTree(t, proto.Guid(hand.GUID),
		wiregen.MustMakeObj("olieodeoaof", map[int]*proto.Value{0: proto.F32(10)}))

	enableReadOnlyDB(t, db)
	c := &Ctx{Sess: newProbeSess(srv), Tree: req, DecodeOK: true, ExpectResponse: true,
		CallID: 41, Log: logging.Discard()}
	if _, ok := hDynamicHanditemThrow(c); !ok {
		t.Fatal("13/5 未处理")
	}
	disableReadOnlyDB(t, db)
	if got := itemStackQty(t, srv, bait.GUID); got != 2 {
		t.Fatalf("扣料失败时装填物应保持 2 份，实得 %d", got)
	}
	if got := srv.handitemLoadedGuid(ctx, hand.GUID); got != bait.GUID {
		t.Fatalf("扣料失败时手持物仍应指向原弹药，实得 %q", shortGUID(got))
	}
	if n := gbCount(t, srv, uid); n != 0 {
		t.Fatalf("扣料失败不该记窝账，实得 %d 批", n)
	}

	// 正对照：写库恢复后同一条路径必须照常扣料并记窝账，证明上面的失败断言
	// 不是因为 handler 根本没走到扣料。
	if _, ok := hDynamicHanditemThrow(c); !ok {
		t.Fatal("13/5 恢复写库后未处理")
	}
	if got := itemStackQty(t, srv, bait.GUID); got != 1 {
		t.Fatalf("写库恢复后应扣掉 1 份（2→1），实得 %d", got)
	}
	if n := gbCount(t, srv, uid); n != 1 {
		t.Fatalf("写库恢复后这一发应记 1 笔窝账，实得 %d 批", n)
	}
}

// TestHandThrowGroundbaitAbortsWhenConsumeFails 12/1 手抛窝料扣料写库失败：
// 不推送更新、不记窝账，库存原样保留。
func TestHandThrowGroundbaitAbortsWhenConsumeFails(t *testing.T) {
	srv, db := newRigTestServer(t)
	setWeatherTestTunable(t, srv, "groundbait_enabled", "1")
	ctx := context.Background()
	const uid = int64(1000001)
	st := srv.stateFor(uid)
	st.mu.Lock()
	st.Loc, st.PosX, st.PosZ, st.HasPos = "gb-test-loc", 60, 60, true
	st.HasCastPos = false
	st.mu.Unlock()
	stack, err := srv.CreateItem(ctx, uid, "feedcompleted", "backpack", 1)
	if err != nil {
		t.Fatalf("创建窝料堆失败: %v", err)
	}
	srv.setStackCount(ctx, stack, 2)
	if cat, ok := srv.itemCategoryOf(ctx, stack); !ok || cat != catGroundbaitMix {
		t.Fatalf("feedcompleted 品类 = %d (ok=%v)，期望手抛窝料 %d", cat, ok, catGroundbaitMix)
	}
	req := rigRequestTree(t, proto.Guid(stack.GUID),
		wiregen.MustMakeObj("olieodeoaof", map[int]*proto.Value{0: proto.F32(6.5)}))
	sess, _ := newTunnelProbe(t, srv)
	sess.Userid = uid

	enableReadOnlyDB(t, db)
	c := &Ctx{Sess: sess, Main: 12, Sub: 1, CallID: 9301, Tree: req,
		DecodeOK: true, ExpectResponse: false, Log: logging.Discard()}
	hHandThrowGroundbait(c)
	disableReadOnlyDB(t, db)
	if got := itemStackQty(t, srv, stack.GUID); got != 2 {
		t.Fatalf("扣料失败时窝料应保持 2 份，实得 %d", got)
	}
	if n := gbDepositCount(t, srv, uid, "9301"); n != 0 {
		t.Fatalf("扣料失败不该记窝账，实得 %d 批", n)
	}

	// 正对照：写库恢复后同一发必须真扣一份并记进窝账。
	hHandThrowGroundbait(c)
	if got := itemStackQty(t, srv, stack.GUID); got != 1 {
		t.Fatalf("写库恢复后手抛应扣掉 1 份（2→1），实得 %d", got)
	}
	if n := gbDepositCount(t, srv, uid, "9301"); n != 1 {
		t.Fatalf("写库恢复后手抛应记 1 笔窝账，实得 %d 批", n)
	}
}
