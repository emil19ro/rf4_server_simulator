package gameserver

// 整件修理 21/3 的计费口径：零件数与倍率都取自服务端事实，
// 请求里的槽位与店铺名只作诊断。

import (
	"math"
	"testing"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// repairRequestWithSlots 构造 21/3 标准形状：
// args(524)[店铺名, nfmkdcbnkhp{物品GUID, 槽位数组, 金币标志, 客户端报价}]。
func repairRequestWithSlots(storeID, guid string, slots []uint32, clientCost uint64) *proto.Value {
	vals := make([]*proto.Value, 0, len(slots))
	for _, slot := range slots {
		vals = append(vals, proto.U32(slot))
	}
	return proto.Args524(proto.StrVal(storeID), wiregen.MustMakeObj("nfmkdcbnkhp", map[int]*proto.Value{
		0: proto.Guid(guid),
		1: proto.Args524(vals...),
		2: proto.U32(0),
		3: proto.U64(clientCost),
	}))
}

// runWholeItemRepair 把物品本体压到指定磨损后执行一次 21/3 整件修理，返回实际扣费。
func runWholeItemRepair(t *testing.T, srv *Server, db *store.DB, guid string, wear float64, req *proto.Value) (int64, bool) {
	t.Helper()
	ctx := t.Context()
	const uid = int64(1000001)
	if err := db.SetItemDurability(ctx, guid, wear); err != nil {
		t.Fatalf("预置磨损失败: %v", err)
	}
	before, err := db.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	_, ok := hWorkshopRepair(false)(&Ctx{
		Sess: newProbeSess(srv), Main: 21, Sub: 3, Tree: req, DecodeOK: true, Log: logging.Discard(),
	})
	after, err := db.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	return before.Silver - after.Silver, ok
}

// newWornReelForBilling 造一只半磨损渔轮并给足余额，返回渔轮与可修零件数。
// 本体磨损后修理工坊才把本体那一格也列入可修零件，零件数要在磨损之后统计。
func newWornReelForBilling(t *testing.T, srv *Server, db *store.DB, silver int64) (*store.Item, int) {
	t.Helper()
	ctx := t.Context()
	const uid = int64(1000001)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBalance(ctx, uid, silver, 0, 0); err != nil {
		t.Fatal(err)
	}
	reel, err := srv.CreateItem(ctx, uid, "spin_5354", "backpack", 1)
	if err != nil {
		t.Fatalf("造轮失败: %v", err)
	}
	if err := db.SetItemDurability(ctx, reel.GUID, 0.5); err != nil {
		t.Fatalf("预置磨损失败: %v", err)
	}
	worn := mustItem(t, db, reel.GUID)
	return worn, len(srv.repairPartsOf(ctx, worn))
}

// TestWorkshopRepairCostIgnoresClientSlots 同一只磨损渔轮用空槽位、单槽位、五槽位
// 三种请求提交，服务端扣费必须一致，且等于按轮内真实零件数计价的结果。
func TestWorkshopRepairCostIgnoresClientSlots(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	reel, parts := newWornReelForBilling(t, srv, db, 100000)
	if parts != 7 {
		t.Fatalf("半磨损渔轮应列出 7 个可修零件（线杯/摩擦片/轴承/摇柄/轮油/齿轮/本体），实得 %d", parts)
	}
	// 典型磨损渔轮：7 个可修零件、50% 磨损、1 图倍率 1.0 → (120+30×7)×0.5 = 165 银。
	const wantCost = int64(165)
	cases := []struct {
		name   string
		slots  []uint32
		client uint64
	}{
		{"空槽位", nil, 0},
		{"单槽位", []uint32{1}, 0},
		{"五槽位", []uint32{1, 2, 3, 4, 5}, 0},
		// 客户端伪造 1 银报价也不能压价：服务端价格是唯一结算依据。
		{"伪造低价报价", nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := repairRequestWithSlots("workshop_01", reel.GUID, tc.slots, tc.client)
			charged, ok := runWholeItemRepair(t, srv, db, reel.GUID, 0.5, req)
			if !ok {
				t.Fatalf("%s 请求的整件修理未处理", tc.name)
			}
			if charged != wantCost {
				t.Fatalf("%s 请求的修理费=%d，期望按 %d 个零件计 %d", tc.name, charged, parts, wantCost)
			}
		})
	}
}

// TestWorkshopRepairPriceBandForTypicalWear 价格手感：典型磨损渔轮（7 个可修零件、
// 50% 磨损）的整机修理费必须落在 1 图 165 银的 ±10% 内，避免按零件计价后失控。
// 旧版单槽位报价为 150 银，因此整机价相对旧口径的涨幅也必须控制在 20% 以内。
func TestWorkshopRepairPriceBandForTypicalWear(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	_, parts := newWornReelForBilling(t, srv, db, 100000)
	if parts != 7 {
		t.Fatalf("半磨损渔轮应有 7 个可修零件，实得 %d", parts)
	}
	cost := repairCost(parts, false, 0.5, repairMultiplierForLocation("workshop_01", spawnLevelKey))
	const legacyCost = int64(150)
	if math.Abs(float64(cost)-165) > 165*0.10 {
		t.Fatalf("典型磨损渔轮整机修理费=%d，超出基准 165 银的 ±10%%", cost)
	}
	if growth := float64(cost-legacyCost) / float64(legacyCost); growth > 0.20 {
		t.Fatalf("典型磨损渔轮整机修理费=%d，比旧版单槽位报价 %d 上涨 %.1f%%，超过 20%%",
			cost, legacyCost, growth*100)
	}
}

// TestWorkshopRepairCostIgnoresClientStoreName 玩家停在 2 图（倍率 0.85）时，
// 请求里换成低价图店铺、跨图店铺或不存在店铺，扣费都必须等于当前地图价格。
func TestWorkshopRepairCostIgnoresClientStoreName(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	reel, parts := newWornReelForBilling(t, srv, db, 100000)
	const level = "level_002_kirzah_river"
	srv.setLevel(1000001, level)
	if mult := repairMultiplierForLocation("", level); math.Abs(mult-0.85) > 1e-9 {
		t.Fatalf("惟有诺克河地图倍率应为 0.85，实得 %v", mult)
	}
	// 7 个可修零件、50% 磨损、惟有诺克河 0.85 → (120+30×7)×0.5×0.85 = 140.25 → 140 银。
	const wantCost = int64(140)
	for _, storeID := range []string{"workshop_01", "workshop_02", "workshop_viiveri", "no_such_store"} {
		t.Run(storeID, func(t *testing.T) {
			req := repairRequestWithSlots(storeID, reel.GUID, []uint32{1}, 0)
			charged, ok := runWholeItemRepair(t, srv, db, reel.GUID, 0.5, req)
			if !ok {
				t.Fatalf("店铺 %q 的整件修理未处理", storeID)
			}
			if charged != wantCost {
				t.Fatalf("店铺 %q 的修理费=%d，期望按玩家当前地图与 %d 个零件计 %d", storeID, charged, parts, wantCost)
			}
		})
	}
}

// TestWorkshopRepairRestoresReelAndChargesFullParts 回归：修理后本体耐久回 1.0、
// 轮内零件全部恢复、余额按真实零件数扣减。
func TestWorkshopRepairRestoresReelAndChargesFullParts(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	const uid = int64(1000001)
	reel, _ := seedWornWorkshopReel(t, srv, db, .8, .2, .5, .5)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBalance(ctx, uid, 100000, 0, 0); err != nil {
		t.Fatal(err)
	}
	parts := len(srv.repairPartsOf(ctx, mustItem(t, db, reel.GUID)))
	wantCost := repairCost(parts, false, 0.5, repairMultiplierForLocation("", spawnLevelKey))
	charged, ok := runWholeItemRepair(t, srv, db, reel.GUID, 0.5,
		repairRequestWithSlots("workshop_02", reel.GUID, nil, 0))
	if !ok {
		t.Fatal("整件修理未处理")
	}
	if charged != wantCost {
		t.Fatalf("按 %d 个零件计的修理费=%d，期望 %d", parts, charged, wantCost)
	}
	fresh := mustItem(t, db, reel.GUID)
	if fresh.Durability != 1 {
		t.Fatalf("修理后本体耐久应为 1.0，实得 %v", fresh.Durability)
	}
	for _, part := range srv.repairPartsOf(ctx, fresh) {
		if part.Cond != 1 {
			t.Fatalf("修理后品类 %d 的零件完好度应为 1，实得 %v", part.Cat, part.Cond)
		}
	}
}

// TestWorkshopRepairRejectsInsufficientBalanceWithoutSideEffects 余额不足时不扣费、
// 不改本体耐久，也不动轮内零件。
func TestWorkshopRepairRejectsInsufficientBalanceWithoutSideEffects(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	const uid = int64(1000001)
	reel, _ := seedWornWorkshopReel(t, srv, db, .8, .2, .5, .5)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	parts := len(srv.repairPartsOf(ctx, mustItem(t, db, reel.GUID)))
	cost := repairCost(parts, false, 0.5, repairMultiplierForLocation("", spawnLevelKey))
	if err := db.UpdateBalance(ctx, uid, cost-1, 0, 0); err != nil {
		t.Fatal(err)
	}
	charged, ok := runWholeItemRepair(t, srv, db, reel.GUID, 0.5,
		repairRequestWithSlots("workshop_01", reel.GUID, nil, 0))
	if !ok {
		t.Fatal("21/3 余额不足应回原物品状态，不能落入兜底")
	}
	if charged != 0 {
		t.Fatalf("余额不足不应扣费，实扣 %d", charged)
	}
	fresh := mustItem(t, db, reel.GUID)
	if fresh.Durability != .5 {
		t.Fatalf("余额不足时本体耐久应保持 0.5，实得 %v", fresh.Durability)
	}
	want := map[uint32]float64{uint32(reelPartCatGear): .8, uint32(reelPartCatWasher): .2, uint32(reelPartCatBearing): .5}
	for _, part := range srv.repairPartsOf(ctx, fresh) {
		if cond, ok := want[part.Cat]; ok && math.Abs(part.Cond-cond) > 1e-6 {
			t.Fatalf("余额不足时品类 %d 的零件完好度应保持 %v，实得 %v", part.Cat, cond, part.Cond)
		}
	}
}

// ---- 真实注册路径（21/4 确认）：走 Dispatch 注册表，不直接调用处理器 ----

// workshopOrderRequest 构造 21/4 请求：args(524)[店铺名, nfmkdcbnkhp{物品GUID, 订单列表, 金币标志, 报价}]。
func workshopOrderRequest(guid string, orders ...*proto.Value) *proto.Value {
	return proto.Args524(proto.StrVal("workshop_01"), wiregen.MustMakeObj("nfmkdcbnkhp", map[int]*proto.Value{
		0: proto.Guid(guid),
		1: proto.Args(proto.S("503"), orders...),
		2: proto.U32(0),
		3: proto.U64(0),
	}))
}

// workshopOrderLine 构造一条订单：f0 零件品类，f1 新件 systemID（与已装同型号即修理）。
func workshopOrderLine(cat int64, systemID string) *proto.Value {
	return wiregen.MustMakeObj("gcllbdkgkna", map[int]*proto.Value{
		0: proto.U32(uint32(cat)),
		1: proto.StrVal(systemID),
	})
}

// runWorkshopOrderViaDispatch 通过真实注册表执行一次 21/4，返回是否处理与银币扣费。
func runWorkshopOrderViaDispatch(t *testing.T, srv *Server, req *proto.Value) (bool, int64) {
	t.Helper()
	ctx := t.Context()
	const uid = int64(1000001)
	d := NewDispatch(srv)
	registerHandlers(d)
	h := d.lookup(21, 4)
	if h == nil {
		t.Fatal("21/4 未注册处理器")
	}
	before, err := srv.DB.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	body, ok := h(&Ctx{Sess: newProbeSess(srv), Main: 21, Sub: 4, Tree: req, DecodeOK: true, Log: logging.Discard()})
	after, err := srv.DB.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	return ok && len(body) > 0, before.Silver - after.Silver
}

// TestWorkshopOrderChargesOnlyOrderedPart 真实 21/4：只下一个内件订单时只扣该零件的钱、
// 只恢复该零件，其它内件保持原耐久。渔轮本体是内件聚合值，按聚合语义重算。
func TestWorkshopOrderChargesOnlyOrderedPart(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	reel, gearID := seedWornWorkshopReel(t, srv, db, .8, .2, .5, .5)
	if _, err := db.EnsureProfile(ctx, 1000001); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBalance(ctx, 1000001, 100000, 0, 0); err != nil {
		t.Fatal(err)
	}
	ok, charged := runWorkshopOrderViaDispatch(t, srv, workshopOrderRequest(reel.GUID,
		workshopOrderLine(reelPartCatGear, gearID)))
	if !ok {
		t.Fatal("只下齿轮订单的 21/4 未处理")
	}
	if want := workshopPartRepairPrice(.8); charged != want {
		t.Fatalf("只修一个内件的扣费=%d，期望该零件报价 %d", charged, want)
	}
	conds := map[uint32]float64{}
	for _, p := range srv.repairPartsOf(ctx, mustItem(t, db, reel.GUID)) {
		conds[p.Cat] = p.Cond
	}
	if conds[uint32(reelPartCatGear)] != 1 {
		t.Fatalf("下单的齿轮应恢复为 1，实得 %v", conds[uint32(reelPartCatGear)])
	}
	if math.Abs(conds[uint32(reelPartCatWasher)]-.2) > 1e-6 || math.Abs(conds[uint32(reelPartCatBearing)]-.5) > 1e-6 {
		t.Fatalf("未下单的内件不应被恢复：摩擦片=%v 轴承=%v",
			conds[uint32(reelPartCatWasher)], conds[uint32(reelPartCatBearing)])
	}
}

// TestWorkshopOrderRejectsBodyOrderOnWornReel 真实 21/4：内件仍有磨损时只下本体订单必须整笔拒绝，
// 不扣费也不改耐久。
func TestWorkshopOrderRejectsBodyOrderOnWornReel(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	reel, _ := seedWornWorkshopReel(t, srv, db, .8, .2, .5, .35)
	if _, err := db.EnsureProfile(ctx, 1000001); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBalance(ctx, 1000001, 100000, 0, 0); err != nil {
		t.Fatal(err)
	}
	before := mustItem(t, db, reel.GUID)
	ok, charged := runWorkshopOrderViaDispatch(t, srv, workshopOrderRequest(reel.GUID,
		workshopOrderLine(gearCatReel, reel.ItemID)))
	if ok {
		t.Fatal("内件仍有磨损时本体订单应被拒绝")
	}
	if charged != 0 {
		t.Fatalf("被拒绝的本体订单不应扣费，实扣 %d", charged)
	}
	after := mustItem(t, db, reel.GUID)
	if after.Durability != before.Durability || after.FieldsJSON != before.FieldsJSON {
		t.Fatalf("被拒绝的本体订单改动了装备：耐久 %v -> %v", before.Durability, after.Durability)
	}
}

// TestWorkshopOrderChargesRodBodyWhenRepairingOnePart 真实 21/4：竿只修一个几乎满耐久的
// 零件时，整件修理会把本体耐久一起回满，因此本体磨损也必须计价，否则付一个零件的钱
// 就能修好整根竿。
func TestWorkshopOrderChargesRodBodyWhenRepairingOnePart(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	rod, handleID := seedWornWorkshopRod(t, srv, db, .99, .01)
	if _, err := db.EnsureProfile(ctx, 1000001); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBalance(ctx, 1000001, 100000, 0, 0); err != nil {
		t.Fatal(err)
	}
	ok, charged := runWorkshopOrderViaDispatch(t, srv, workshopOrderRequest(rod.GUID,
		workshopOrderLine(4, handleID)))
	if !ok {
		t.Fatal("竿柄订单的 21/4 未处理")
	}
	// 竿柄 1% 磨损 + 本体 99% 磨损都要收：整件修理把本体一并修回 1.0。
	want := workshopPartRepairPrice(.99) + workshopPartRepairPrice(.01)
	if charged != want {
		t.Fatalf("只修竿柄的扣费=%d，期望竿柄与本体磨损合计 %d", charged, want)
	}
	if fresh := mustItem(t, db, rod.GUID); fresh.Durability != 1 {
		t.Fatalf("整件修理后本体耐久应为 1，实得 %v", fresh.Durability)
	}
}

// TestWorkshopOrderFullRodRepairChargesEveryWornPart 真实 21/4：竿的三个零件与本体同时磨损，
// 整件修理的扣费必须覆盖全部磨损，修完后零件与本体都回满。
func TestWorkshopOrderFullRodRepairChargesEveryWornPart(t *testing.T) {
	srv, db := newStaticRigTestServer(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	rod, handleID := seedWornWorkshopRod(t, srv, db, .4, .5)
	fields := snapshotFields(t, mustItem(t, db, rod.GUID).FieldsJSON)
	guide, guideDef := snapshotPart(t, srv, fields, 5)
	blank, blankDef := snapshotPart(t, srv, fields, 36)
	setWireFloat(guide.Fields, guide.Class, fieldIdxByCS(guide.Class, csDurability), .3)
	setWireFloat(blank.Fields, blank.Class, fieldIdxByCS(blank.Class, csDurability), .1)
	row := mustItem(t, db, rod.GUID)
	if err := db.UpdateItemFields(ctx, rod.GUID, snapshotJSON(t, fields), row.Qty, row.Durability, row.Workmanship, row.RigGUID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureProfile(ctx, 1000001); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBalance(ctx, 1000001, 100000, 0, 0); err != nil {
		t.Fatal(err)
	}
	ok, charged := runWorkshopOrderViaDispatch(t, srv, workshopOrderRequest(rod.GUID,
		workshopOrderLine(4, handleID),
		workshopOrderLine(5, guideDef.ItemID),
		workshopOrderLine(36, blankDef.ItemID)))
	if !ok {
		t.Fatal("竿整件修理的 21/4 未处理")
	}
	want := workshopPartRepairPrice(.4) + workshopPartRepairPrice(.3) + workshopPartRepairPrice(.1) +
		workshopPartRepairPrice(.5)
	if charged != want {
		t.Fatalf("竿整件修理扣费=%d，期望三个零件与本体磨损合计 %d", charged, want)
	}
	fresh := mustItem(t, db, rod.GUID)
	if fresh.Durability != 1 {
		t.Fatalf("整件修理后本体耐久应为 1，实得 %v", fresh.Durability)
	}
	for _, p := range srv.repairPartsOf(ctx, fresh) {
		if p.Cond != 1 {
			t.Fatalf("整件修理后品类 %d 的零件应回满，实得 %v", p.Cat, p.Cond)
		}
	}
}
