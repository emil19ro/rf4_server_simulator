package gameserver

import (
	"testing"

	"rf4server/internal/proto"
)

// 货架库存（22/1 的 f3）是展示量：本地商店持续供货，购买只校验价格并扣款，
// 既不读取也不扣减它。tungus_bot 的基础库存来自逐店目录的 1 件。
func TestStoreShelfStockIsDisplayOnlyForRepeatedPurchase(t *testing.T) {
	srv, db := newTungusShopTestServer(t)
	body, ok := hStoreList(economyTestCtx(srv, tungusShopTestUID, proto.Args524(proto.StrVal("tungus_015"))))
	if !ok {
		t.Fatal("22/1 未返回货架")
	}
	shelf, _, err := proto.DecodeValue(body, 0, srv.Table())
	if err != nil {
		t.Fatal(err)
	}
	if stock := shelfStockOf(t, shelf, "tungus_bot"); stock != 1 {
		t.Fatalf("货架库存应展示逐店目录的 1 件，实际 %d", stock)
	}
	for i := 0; i < 2; i++ {
		result := srv.dispatch.executeRequest(tungusShopBuyRequest(srv, "tungus_bot", 3), true)
		if result.failed || result.nativeFailure || !result.hasResponse {
			t.Fatalf("第 %d 次购买应成功：库存只是展示量，不限制购买: %+v", i+1, result)
		}
	}
	// 两次购买各自扣 22 金币并累计 4 件（每件产 2）：库存 1 不构成限购。
	assertTungusShopBalances(t, db, 1000-2*22, 2, 1)
	var qty int64
	if err := db.SQL.QueryRow(`SELECT SUM(qty) FROM item WHERE uid=? AND item_id='tungus_bot'`, tungusShopTestUID).Scan(&qty); err != nil {
		t.Fatal(err)
	}
	if qty != 4 {
		t.Fatalf("两次购买的物品数量=%d want=4", qty)
	}
	body, ok = hStoreList(economyTestCtx(srv, tungusShopTestUID, proto.Args524(proto.StrVal("tungus_015"))))
	if !ok {
		t.Fatal("购买后 22/1 未返回货架")
	}
	shelf, _, err = proto.DecodeValue(body, 0, srv.Table())
	if err != nil {
		t.Fatal(err)
	}
	if stock := shelfStockOf(t, shelf, "tungus_bot"); stock != 1 {
		t.Fatalf("购买后货架库存仍应展示 1 件，实际 %d", stock)
	}
}

// 零库存只补满展示值，不影响成交：覆盖库存为 0 时 22/1 显示补满值，
// 购买照常扣款入库。
func TestStorePurchaseIgnoresZeroDisplayStock(t *testing.T) {
	srv, db := newTungusShopTestServer(t)
	if err := db.SetAdminShopOverride(t.Context(), "tungus_015", "tungus_12pack", 0, 129, 0, "test"); err != nil {
		t.Fatal(err)
	}
	body, ok := hStoreList(economyTestCtx(srv, tungusShopTestUID, proto.Args524(proto.StrVal("tungus_015"))))
	if !ok {
		t.Fatal("22/1 未返回货架")
	}
	shelf, _, err := proto.DecodeValue(body, 0, srv.Table())
	if err != nil {
		t.Fatal(err)
	}
	if stock := shelfStockOf(t, shelf, "tungus_12pack"); stock != 1000 {
		t.Fatalf("零库存应补满为展示值 1000，实际 %d", stock)
	}
	result := srv.dispatch.executeRequest(tungusShopBuyRequest(srv, "tungus_12pack", 3), true)
	if result.failed || result.nativeFailure || !result.hasResponse {
		t.Fatalf("展示库存为补满值时购买应成功: %+v", result)
	}
	assertTungusShopBalances(t, db, 1000-129, 1, 1)
}

// shelfStockOf 读取 22/1 货架里指定商品的 f3 展示库存。
func shelfStockOf(t *testing.T, shelf *proto.Value, itemID string) uint64 {
	t.Helper()
	for i := 0; i < shelf.ArgCount(); i++ {
		row := shelf.ArgItem(i)
		id := row.Field(0)
		if id.StrOr() != itemID {
			continue
		}
		stock := row.Field(3)
		return stock.U64()
	}
	t.Fatalf("货架缺少 %s", itemID)
	return 0
}
