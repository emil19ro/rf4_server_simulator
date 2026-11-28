-- 成就 210 的判据是本杆入水确实消耗掉 PVA 棒。服务端在咬钩前重启时
-- fishing_state 会恢复同一杆的 timer/pending，PVA 标记也必须一起恢复。
ALTER TABLE fishing_state ADD COLUMN pva_ready INTEGER NOT NULL DEFAULT 0;
