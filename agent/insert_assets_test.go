package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// testDB opens a DB connection, skipping if PG is unavailable.
// testDB 打开一个测试用 DB，并注册统一的收尾清理。
//
// # 为什么在 helper 里统一清 notification_events
//
// `report_finding` / `AddFinding` / `FinishFindingRetest` 都会在**同一事务里**
// 往 notification_events 写一条推送事件，而那张表**没有外键** ——
// DeleteTask / DeleteFinding 都不会级联删它。
//
// 于是任何「建了 finding」的测试都会残留事件。逐个测试加 cleanup 会漏
// （本包已因这个泄漏让残留守卫报了 4 行）。放在 helper 里是单点收口：
// 只要用了 testDB，收尾就自动清。
func testDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v)", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// 记录本测试开始前的事件 id 上界，收尾时删掉超过它的一切。
	//
	// # 为什么用 id 上界而不是「按 finding 删」
	//
	// findings.task_id 是 ON DELETE SET NULL —— 删 task 时 finding 保留
	// （漏洞有审计价值，这是设计），所以「finding 已不存在」永远不成立，
	// 那条 DELETE 是死代码。
	//
	// 本包测试串行（TestMain 持 advisory lock），所以 id 上界是可靠的分界。
	var eventMax int64
	_ = d.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&eventMax)
	// findings 同样用 id 上界清。
	//
	// # 为什么不能靠「删 task」带掉 findings
	//
	// findings.task_id 是 ON DELETE SET NULL —— 删 task 时 finding 会保留
	// （漏洞有审计价值，这是有意的设计）。所以任何「建了 finding」的测试
	// 都必须自己清，否则 findings 表会永久累积。
	//
	// 实测（2026-10-30）：本包跑一次让 findings 涨 4 条，而原先只清了
	// notification_events —— 两者是同一类泄漏（测试经 AddFinding 间接写、
	// 而删 task 带不掉），只是 findings 被漏了。
	var findingMax int64
	_ = d.QueryRow(`SELECT COALESCE(max(id),0) FROM findings`).Scan(&findingMax)

	t.Cleanup(func() {
		// # 为什么用一条独立连接清理，而不是用 d
		//
		// 调用方通常写 `t.Cleanup(func() { d.Close() })`，而 t.Cleanup 是 LIFO ——
		// 后注册的先执行。所以 d.Close() 跑在**本函数注册的 cleanup 之前**，
		// 那时 d 已关闭，DELETE 必然报 `sql: database is closed`。
		//
		// # 为什么不靠「调整注册顺序」修
		//
		// 那要求每个调用方都理解 LIFO，并保证「先注册清理、后注册 Close」。
		// 44 个调用点里任何一个写反，问题就复发 —— 这是「依赖顺序正确」的脆弱方案。
		//
		// # 独立连接完全不依赖顺序
		//
		// 无论 d 是否已被关闭，这条清理都能执行。
		cleanupDB, cErr := db.Open(dsn)
		if cErr != nil {
			// 清理失败是测试基础设施故障，不该静默（原来用 t.Logf 会吞掉它）。
			t.Errorf("清理 notification_events：重开连接失败: %v", cErr)
			return
		}
		defer func() { _ = cleanupDB.Close() }()

		if _, err := cleanupDB.Exec(`DELETE FROM notification_events WHERE id > $1`, eventMax); err != nil {
			// 只报「DELETE 报错」。「影响 0 行」不是失败 ——
			// 那可能是这条测试本来就没建 notification_events。
			t.Errorf("清理 notification_events 失败: %v", err)
		}
		// 顺序：findings 的子表（retests / bindings）是 ON DELETE CASCADE，
		// 所以只需删 findings 本表。
		if _, err := cleanupDB.Exec(`DELETE FROM findings WHERE id > $1`, findingMax); err != nil {
			t.Errorf("清理 findings 失败: %v", err)
		}
	})
	return d
}

// callInsertAssets calls the insert_assets tool with the given payload.
func callInsertAssets(t *testing.T, ts *ToolSet, payload any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(payload)
	tool := ts.insertAssets()
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("insertAssets Call error: %v", err)
	}
	text := res.Flatten()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal result: %v\nraw: %s", err, text)
	}
	return out
}

// =====================================================================
// TestInsertAssetsSubdomainSideEffects
// 子域名插入 → 自动创建 root_domain + IP 资产，IP 绑定域名
// =====================================================================
func TestInsertAssetsSubdomainSideEffects(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	ts := NewToolSet(nil, "")
	ts.SetAssetStore(d.Assets(), d.Companies())
	defer d.Exec(`DELETE FROM assets WHERE domain IN ('ia-sub.sideeffect-test.com','sideeffect-test.com') OR ip='7.8.9.10'`)

	out := callInsertAssets(t, ts, map[string]any{
		"assets": []any{
			map[string]any{
				"type":         "subdomain",
				"domain":       "ia-sub.sideeffect-test.com",
				"record_type":  "A",
				"record_value": []string{"7.8.9.10"},
			},
		},
		"task_id": 999,
	})

	// no errors
	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	results, _ := out["results"].([]any)
	if len(results) == 0 {
		t.Fatal("no results returned")
	}

	// root_domain should exist
	var rootCnt int
	d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type='root_domain' AND domain='sideeffect-test.com'`).Scan(&rootCnt)
	if rootCnt != 1 {
		t.Errorf("side-effect: root_domain not created, got %d", rootCnt)
	}

	// IP asset should exist with bound_domains containing our subdomain
	var ipID int64
	var boundDomains []byte
	d.QueryRow(`SELECT id, COALESCE(bound_domains,'[]') FROM assets WHERE type='ip' AND ip='7.8.9.10'`).Scan(&ipID, &boundDomains)
	if ipID == 0 {
		t.Error("side-effect: IP asset not created")
	}
	var domains []string
	json.Unmarshal(boundDomains, &domains)
	found := false
	for _, d := range domains {
		if d == "ia-sub.sideeffect-test.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("side-effect: bound_domains should contain subdomain, got %v", domains)
	}

	// record_value stored as array
	var rvRaw []byte
	d.QueryRow(`SELECT COALESCE(record_value,'[]') FROM assets WHERE type='subdomain' AND domain='ia-sub.sideeffect-test.com'`).Scan(&rvRaw)
	var rv []string
	json.Unmarshal(rvRaw, &rv)
	if len(rv) == 0 || rv[0] != "7.8.9.10" {
		t.Errorf("record_value stored incorrectly: %v", rv)
	}
}

// =====================================================================
// TestInsertAssetsMultiIPSubdomain
// 多个 IP 的子域名：所有 IP 都应存入 record_value[]，各自创建 IP 资产
// =====================================================================
func TestInsertAssetsMultiIPSubdomain(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	ts := NewToolSet(nil, "")
	ts.SetAssetStore(d.Assets(), d.Companies())
	defer d.Exec(`DELETE FROM assets WHERE domain IN ('multi.multiip-test.io','multiip-test.io') OR ip IN ('1.1.1.1','2.2.2.2')`)

	out := callInsertAssets(t, ts, map[string]any{
		"assets": []any{
			map[string]any{
				"type":         "subdomain",
				"domain":       "multi.multiip-test.io",
				"record_type":  "A",
				"record_value": []string{"1.1.1.1", "2.2.2.2"},
			},
		},
	})

	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	// Both IPs should have IP assets
	var ip1Cnt, ip2Cnt int
	d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type='ip' AND ip='1.1.1.1'`).Scan(&ip1Cnt)
	d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type='ip' AND ip='2.2.2.2'`).Scan(&ip2Cnt)
	if ip1Cnt != 1 {
		t.Error("IP 1.1.1.1 asset not created")
	}
	if ip2Cnt != 1 {
		t.Error("IP 2.2.2.2 asset not created")
	}

	// record_value should contain both IPs
	var rvRaw []byte
	d.QueryRow(`SELECT COALESCE(record_value,'[]') FROM assets WHERE type='subdomain' AND domain='multi.multiip-test.io'`).Scan(&rvRaw)
	var rv []string
	json.Unmarshal(rvRaw, &rv)
	if len(rv) != 2 {
		t.Errorf("record_value: want 2 IPs, got %v", rv)
	}
}

// =====================================================================
// TestInsertAssetsHTTPServiceTechnologies
// HTTP 服务插入：technologies 存储并可读回；IP 存在时域名和端口写入 IP 资产
// =====================================================================
func TestInsertAssetsHTTPServiceTechnologies(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	ts := NewToolSet(nil, "")
	ts.SetAssetStore(d.Assets(), d.Companies())
	defer d.Exec(`DELETE FROM assets WHERE url='https://tech-test.example.com' OR domain IN ('tech-test.example.com','example.com') OR ip='3.4.5.6'`)

	out := callInsertAssets(t, ts, map[string]any{
		"assets": []any{
			map[string]any{
				"type":         "service",
				"url":          "https://tech-test.example.com",
				"service_ip":   "3.4.5.6",
				"technologies": []string{"Nginx", "Vue.js", "Cloudflare"},
				"status_code":  200,
				"page_title":   "Tech Test Site",
			},
		},
		"task_id": 888,
	})

	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	// technologies should be stored
	var techCnt int
	d.QueryRow(`SELECT json_array_length(technologies) FROM assets WHERE url='https://tech-test.example.com'`).Scan(&techCnt)
	if techCnt != 3 {
		t.Errorf("technologies: want 3, got %d", techCnt)
	}

	// QueryByType should return technologies correctly (verifies JSON array scan)
	assets, err := d.Assets().QueryByType("service", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found *db.Asset
	for _, a := range assets {
		if a.URL == "https://tech-test.example.com" {
			found = a
			break
		}
	}
	if found == nil {
		t.Fatal("service not found via QueryByType")
	}
	if len(found.Technologies) != 3 {
		t.Errorf("QueryByType: technologies roundtrip failed, got %v", found.Technologies)
	}

	// side effect: IP asset should exist with bound_domains containing the service domain
	var ipID int64
	var bdRaw []byte
	var portCnt int
	d.QueryRow(`SELECT id, COALESCE(bound_domains,'[]') FROM assets WHERE type='ip' AND ip='3.4.5.6'`).Scan(&ipID, &bdRaw)
	if ipID == 0 {
		t.Error("side-effect: IP asset not created for HTTP service IP")
	}
	var bd []string
	json.Unmarshal(bdRaw, &bd)
	hasDomain := false
	for _, dom := range bd {
		if dom == "tech-test.example.com" {
			hasDomain = true
		}
	}
	if !hasDomain {
		t.Errorf("side-effect: IP bound_domains missing service domain, got %v", bd)
	}

	// side effect: IP open_ports should contain port 443
	d.QueryRow(`SELECT json_array_length(open_ports) FROM assets WHERE type='ip' AND ip='3.4.5.6'`).Scan(&portCnt)
	if portCnt == 0 {
		t.Error("side-effect: IP open_ports not set for HTTP service")
	}
}

// =====================================================================
// TestInsertAssetsOtherService
// 非 HTTP 服务：c_segment 自动生成，IP 资产含 open_ports 和 bound_domains
// =====================================================================
func TestInsertAssetsOtherService(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	ts := NewToolSet(nil, "")
	ts.SetAssetStore(d.Assets(), d.Companies())
	defer d.Exec(`DELETE FROM assets WHERE
		(type='service' AND ip='10.20.30.40') OR
		(type='ip' AND ip='10.20.30.40') OR
		domain IN ('db.othersvc-test.com','othersvc-test.com')`)

	out := callInsertAssets(t, ts, map[string]any{
		"assets": []any{
			map[string]any{
				"type":         "service",
				"ip":           "10.20.30.40",
				"domain":       "db.othersvc-test.com",
				"port":         3306,
				"service_name": "mysql",
			},
		},
	})

	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	// c_segment should be auto-set on the service
	var cseg *string
	d.QueryRow(`SELECT c_segment FROM assets WHERE type='service' AND ip='10.20.30.40'`).Scan(&cseg)
	if cseg == nil || *cseg != "10.20.30.0/24" {
		t.Errorf("c_segment: want 10.20.30.0/24, got %v", cseg)
	}

	// IP side-effect: port 3306 in open_ports
	var portCnt int
	d.QueryRow(`SELECT json_array_length(open_ports) FROM assets WHERE type='ip' AND ip='10.20.30.40'`).Scan(&portCnt)
	if portCnt == 0 {
		t.Error("side-effect: IP open_ports should contain port 3306")
	}

	// IP side-effect: bound_domains contains the service domain
	var bdRaw []byte
	d.QueryRow(`SELECT COALESCE(bound_domains,'[]') FROM assets WHERE type='ip' AND ip='10.20.30.40'`).Scan(&bdRaw)
	var bd []string
	json.Unmarshal(bdRaw, &bd)
	hasDomain := false
	for _, dom := range bd {
		if dom == "db.othersvc-test.com" {
			hasDomain = true
		}
	}
	if !hasDomain {
		t.Errorf("side-effect: IP bound_domains missing service domain, got %v", bd)
	}
}

// =====================================================================
// TestInsertAssetsMixedBatch
// 混合批量插入：一次调用插入多种类型
// =====================================================================
func TestInsertAssetsMixedBatch(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	ts := NewToolSet(nil, "")
	ts.SetAssetStore(d.Assets(), d.Companies())
	defer d.Exec(`DELETE FROM assets WHERE
		domain IN ('batch-sub.batch-test.org','batch-test.org') OR
		ip='55.66.77.88' OR
		url='https://batch-test.org/api' OR
		(type='endpoint' AND url='https://batch-test.org/api/users')`)

	out := callInsertAssets(t, ts, map[string]any{
		"assets": []any{
			// root_domain
			map[string]any{"type": "root_domain", "domain": "batch-test.org"},
			// subdomain with A record
			map[string]any{"type": "subdomain", "domain": "batch-sub.batch-test.org", "record_type": "A", "record_value": []string{"55.66.77.88"}},
			// HTTP service
			map[string]any{"type": "service", "url": "https://batch-test.org/api", "technologies": []string{"Go", "PostgreSQL"}, "status_code": 200},
			// endpoint
			map[string]any{"type": "endpoint", "url": "https://batch-test.org/api/users", "method": "GET"},
		},
		"task_id": 777,
	})

	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	results, _ := out["results"].([]any)
	if len(results) != 4 {
		t.Errorf("mixed batch: want 4 results, got %d", len(results))
	}

	// verify all types exist in DB
	types := []string{"root_domain", "subdomain", "service", "endpoint"}
	for _, typ := range types {
		var cnt int
		switch typ {
		case "root_domain":
			d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type=$1 AND domain='batch-test.org'`, typ).Scan(&cnt)
		case "subdomain":
			d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type=$1 AND domain='batch-sub.batch-test.org'`, typ).Scan(&cnt)
		case "service":
			d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type=$1 AND url='https://batch-test.org/api'`, typ).Scan(&cnt)
		case "endpoint":
			d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type=$1 AND url='https://batch-test.org/api/users'`, typ).Scan(&cnt)
		}
		if cnt != 1 {
			t.Errorf("mixed batch: %s not found in DB", typ)
		}
	}
}

// =====================================================================
// TestInsertAssetsDedup
// 幂等写入：同一资产插入两次，返回相同 ID
// =====================================================================
func TestInsertAssetsDedup(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	ts := NewToolSet(nil, "")
	ts.SetAssetStore(d.Assets(), d.Companies())
	defer d.Exec(`DELETE FROM assets WHERE domain='dedup-ia.deduptest.net' OR domain='deduptest.net'`)

	payload := map[string]any{
		"assets": []any{
			map[string]any{"type": "root_domain", "domain": "deduptest.net"},
		},
	}

	out1 := callInsertAssets(t, ts, payload)
	out2 := callInsertAssets(t, ts, payload)

	getID := func(out map[string]any) float64 {
		results, _ := out["results"].([]any)
		if len(results) == 0 {
			return 0
		}
		m, _ := results[0].(map[string]any)
		id, _ := m["id"].(float64)
		return id
	}

	id1, id2 := getID(out1), getID(out2)
	if id1 == 0 || id1 != id2 {
		t.Errorf("dedup: want same ID on double insert, got %v vs %v", id1, id2)
	}
}

// =====================================================================
// TestInsertAssetsRejectsHostnameIPPerItem
// 一批里混入 ip 填了主机名的一条 → 只有那条失败，其余照常入库，
// 且错误里带得上 index 和改正方法，Agent 下一轮能自己修好。
// =====================================================================
func TestInsertAssetsRejectsHostnameIPPerItem(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	ts := NewToolSet(nil, "")
	ts.SetAssetStore(d.Assets(), d.Companies())
	defer d.Exec(`DELETE FROM assets WHERE ip IN ('198.51.100.23','cdn.badip-test.com') OR domain='badip-test.com'`)

	out := callInsertAssets(t, ts, map[string]any{
		"assets": []any{
			map[string]any{"type": "root_domain", "domain": "badip-test.com"},
			map[string]any{"type": "ip", "ip": "cdn.badip-test.com"},
			map[string]any{"type": "ip", "ip": "198.51.100.23"},
		},
	})

	// The two valid entries must survive the bad one — a whole-batch failure
	// would make the agent re-send assets that were already fine.
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results=%v, want the 2 valid assets", out["results"])
	}

	errsRaw, _ := out["errors"].([]any)
	if len(errsRaw) != 1 {
		t.Fatalf("errors=%v, want exactly the invalid entry", out["errors"])
	}
	entry, _ := errsRaw[0].(map[string]any)
	if index, _ := entry["index"].(float64); int(index) != 1 {
		t.Fatalf("error index=%v, want 1", entry["index"])
	}
	message, _ := entry["error"].(string)
	for _, want := range []string{"cdn.badip-test.com", "type=subdomain", "A/AAAA"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error message %q lacks %q — agent cannot act on it", message, want)
		}
	}

	// The rejected value must not have reached the table.
	var stored int
	if err := d.QueryRow(`SELECT count(*) FROM assets WHERE ip='cdn.badip-test.com'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("rejected hostname still stored in assets.ip (%d rows)", stored)
	}
}
