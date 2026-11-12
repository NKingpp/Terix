package db

// T02 SQLite 迁移的回归测试：资产/公司域的方言改写。
//
// 覆盖三块最容易改错、且 PG 与 SQLite 语义差异最大的地方：
//  1. DISTINCT ON → ROW_NUMBER 的等价性（open_ports 端口去重、params 去重、
//     企业归属的最长前缀/最长域名优先）
//  2. cidr_contains 的边界（相等 / 包含 / 不包含 / 非法值返回 NULL）
//  3. asset_dsl 的模糊搜索与数组包含（LIKE 替代 ILIKE、json_each 替代 unnest）
//
// 全部用真库（tempdir 文件），因为多数缺陷只在 SQL 真正执行时才暴露。

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func t02Setup(t *testing.T) (*DB, *AssetStore) {
	t.Helper()
	d, err := Open(t.TempDir() + "/t02.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, d.Assets()
}

// newTask 建一个带 exploration 的最小任务，返回 task id。
func t02NewTask(t *testing.T, d *DB, goal string) int64 {
	t.Helper()
	var expID int64
	if err := d.QueryRow(`INSERT INTO explorations(goal) VALUES($1) RETURNING id`, goal).Scan(&expID); err != nil {
		t.Fatalf("insert exploration: %v", err)
	}
	var taskID int64
	if err := d.QueryRow(`
INSERT INTO tasks(description, goal, status, exploration_id)
VALUES('', $1, 'running', $2) RETURNING id`, goal, expID).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return taskID
}

// ---------------------------------------------------------------------------
// 1. DISTINCT ON → ROW_NUMBER：open_ports 按端口去重，同端口优先保留 service 非空
// ---------------------------------------------------------------------------

func TestT02OpenPortsDedupPrefersService(t *testing.T) {
	d, assets := t02Setup(t)
	taskID := t02NewTask(t, d, "open-ports-dedup")

	// 第一次写入只带空 service，第二次带真 service —— 合并后必须留下后者。
	if _, err := assets.UpsertIP(UpsertIPReq{
		IP:        "10.9.0.1",
		OpenPorts: []PortService{{Port: 80, Service: ""}, {Port: 22, Service: "ssh"}},
		TaskID:    taskID,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if _, err := assets.UpsertIP(UpsertIPReq{
		IP:        "10.9.0.1",
		OpenPorts: []PortService{{Port: 80, Service: "http"}, {Port: 443, Service: "https"}},
		TaskID:    taskID,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	list, err := assets.QueryByType("ip", 10, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 ip asset, got %d", len(list))
	}
	ports := list[0].OpenPorts
	if len(ports) != 3 {
		t.Fatalf("want 3 deduped ports (22,80,443), got %d: %+v", len(ports), ports)
	}
	// 端口升序（原 PG 的 ORDER BY (elem->>'port')::int）
	wantOrder := []int{22, 80, 443}
	for i, want := range wantOrder {
		got, ok := ports[i]["port"].(float64)
		if !ok {
			t.Fatalf("port[%d] is not a number: %+v", i, ports[i])
		}
		if int(got) != want {
			t.Fatalf("port[%d] = %v, want %d (order preserved)", i, got, want)
		}
	}
	// 同端口去重后必须保住 service 非空的那条 —— DISTINCT ON 语义的核心。
	for _, p := range ports {
		if int(p["port"].(float64)) == 80 {
			if svc, _ := p["service"].(string); svc != "http" {
				t.Fatalf("port 80 kept wrong row: service=%q, want http", svc)
			}
		}
	}
}

// 输入顺序无关：先给真 service 再给空 service，结果必须一致。
func TestT02OpenPortsDedupOrderIndependent(t *testing.T) {
	d, assets := t02Setup(t)
	taskID := t02NewTask(t, d, "open-ports-order")

	if _, err := assets.UpsertIP(UpsertIPReq{
		IP:        "10.9.0.2",
		OpenPorts: []PortService{{Port: 8080, Service: "http-proxy"}},
		TaskID:    taskID,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := assets.UpsertIP(UpsertIPReq{
		IP:        "10.9.0.2",
		OpenPorts: []PortService{{Port: 8080, Service: ""}},
		TaskID:    taskID,
	}); err != nil {
		t.Fatalf("second: %v", err)
	}
	list, err := assets.QueryByType("ip", 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("query: n=%d err=%v", len(list), err)
	}
	if svc, _ := list[0].OpenPorts[0]["service"].(string); svc != "http-proxy" {
		t.Fatalf("service downgraded to %q; dedup must keep the non-empty one", svc)
	}
}

// ---------------------------------------------------------------------------
// 2. DISTINCT ON → ROW_NUMBER：endpoint params 按 (location,name) 去重
// ---------------------------------------------------------------------------

func TestT02EndpointParamsDedup(t *testing.T) {
	d, assets := t02Setup(t)
	taskID := t02NewTask(t, d, "params-dedup")

	url := "https://api.t02.example.com/v1/users"
	if _, err := assets.UpsertEndpoint(UpsertEndpointReq{
		URL:    url,
		Method: "get",
		Params: []map[string]any{
			{"location": "query", "name": "page", "type": "int"},
			{"location": "query", "name": "size", "type": "int"},
		},
		TaskID: taskID,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := assets.UpsertEndpoint(UpsertEndpointReq{
		URL:    url,
		Method: "GET", // 大小写归一后同一个 key
		Params: []map[string]any{
			{"location": "query", "name": "page", "value": "2"},
			{"location": "header", "name": "X-Token"},
		},
		TaskID: taskID,
	}); err != nil {
		t.Fatalf("second: %v", err)
	}

	list, err := assets.QueryByType("endpoint", 10, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 endpoint, got %d (method 归一失败会变 2 条)", len(list))
	}
	params := list[0].Params
	if len(params) != 3 {
		t.Fatalf("want 3 params (query/page, query/size, header/X-Token), got %d: %+v", len(params), params)
	}
	seen := map[string]bool{}
	for _, p := range params {
		key := fmt.Sprint(p["location"]) + "/" + fmt.Sprint(p["name"])
		if seen[key] {
			t.Fatalf("duplicate param key after merge: %s", key)
		}
		seen[key] = true
	}
	for _, want := range []string{"query/page", "query/size", "header/X-Token"} {
		if !seen[want] {
			t.Fatalf("missing param %s, got %v", want, seen)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. 数组列的追加/去重（json_group_array 替代 ARRAY(SELECT DISTINCT unnest(...))）
// ---------------------------------------------------------------------------

func TestT02TaskIDsAppendDedup(t *testing.T) {
	d, assets := t02Setup(t)
	taskA := t02NewTask(t, d, "task-ids-a")
	taskB := t02NewTask(t, d, "task-ids-b")

	if _, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "t02-arrays.example.com", TaskID: taskA}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// 重复同一个 task 必须不产生重复元素（NOT NULL DEFAULT '[]' + UNIQUE 去重）
	if _, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "t02-arrays.example.com", TaskID: taskA}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if _, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "t02-arrays.example.com", TaskID: taskB}); err != nil {
		t.Fatalf("second task: %v", err)
	}

	var raw string
	if err := d.QueryRow(`SELECT task_ids FROM assets WHERE domain='t02-arrays.example.com'`).Scan(&raw); err != nil {
		t.Fatalf("read task_ids: %v", err)
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatalf("task_ids is not JSON array: %q (%v)", raw, err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 distinct task ids, got %v", ids)
	}

	// 去重后仍要能按 task 反查
	got, err := assets.QueryByTask(taskB, "", 10, 0)
	if err != nil {
		t.Fatalf("QueryByTask: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("QueryByTask(%d) = %d rows, want 1", taskB, len(got))
	}
	if n, err := assets.CountByTask(taskB, ""); err != nil || n != 1 {
		t.Fatalf("CountByTask = %d, err=%v", n, err)
	}
	// 其它 task 不应命中
	if n, _ := assets.CountByTask(taskA+taskB+1000, ""); n != 0 {
		t.Fatalf("unrelated task matched %d rows", n)
	}
}

// DetachAssetFromTask 用的是 array_remove 的等价物，必须精确删一个元素。
func TestT02DetachKeepsOtherTaskIDs(t *testing.T) {
	d, assets := t02Setup(t)
	taskA := t02NewTask(t, d, "detach-a")
	taskB := t02NewTask(t, d, "detach-b")

	id, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "t02-detach.example.com", TaskID: taskA})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "t02-detach.example.com", TaskID: taskB}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	detached, err := assets.DetachAssetFromTask(taskA, id)
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if !detached {
		t.Fatal("detach reported false")
	}
	var raw string
	if err := d.QueryRow(`SELECT task_ids FROM assets WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatalf("read: %v", err)
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatalf("task_ids not JSON: %q", raw)
	}
	if len(ids) != 1 || ids[0] != taskB {
		t.Fatalf("after detach want [%d], got %v", taskB, ids)
	}
	// 再 detach 一次 → 空数组（不是 NULL，也不能报错）
	if _, err := assets.DetachAssetFromTask(taskB, id); err != nil {
		t.Fatalf("detach B: %v", err)
	}
	if err := d.QueryRow(`SELECT task_ids FROM assets WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatalf("read2: %v", err)
	}
	if raw != "[]" {
		t.Fatalf("empty task_ids must be '[]' (json_group_array returns NULL on empty set), got %q", raw)
	}
}

// ---------------------------------------------------------------------------
// 4. cidr_contains 边界：相等 / 包含 / 不包含 / 非法
// ---------------------------------------------------------------------------

func TestT02CIDRContainsBoundaries(t *testing.T) {
	d, _ := t02Setup(t)
	cases := []struct {
		name string
		net  string
		addr string
		want string // "1" / "0" / "NULL"
	}{
		{"exact-v4", "10.0.0.0/32", "10.0.0.0", "1"},
		{"inside", "10.0.0.0/8", "10.200.3.4", "1"},
		{"outside", "10.0.0.0/8", "11.0.0.1", "0"},
		{"boundary-low", "10.0.0.0/24", "10.0.0.0", "1"},
		{"boundary-high", "10.0.0.0/24", "10.0.0.255", "1"},
		{"just-outside", "10.0.0.0/24", "10.0.1.0", "0"},
		{"exact-v6", "2001:db8::/32", "2001:db8::1", "1"},
		{"outside-v6", "2001:db8::/32", "2001:db9::1", "0"},
		{"bad-net", "not-a-net", "10.0.0.1", "NULL"},
		{"bad-addr", "10.0.0.0/8", "hostname.example.com", "NULL"},
		{"bare-addr-as-net", "10.0.0.1", "10.0.0.1", "NULL"}, // PG 的 try_inet 语义：无掩码不当网段
		{"empty-both", "", "", "NULL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got any
			if err := d.QueryRow(`SELECT cidr_contains($1, $2)`, tc.net, tc.addr).Scan(&got); err != nil {
				t.Fatalf("cidr_contains(%q,%q): %v", tc.net, tc.addr, err)
			}
			render := "NULL"
			if got != nil {
				render = fmt.Sprint(got)
			}
			if render != tc.want {
				t.Fatalf("cidr_contains(%q,%q) = %s, want %s", tc.net, tc.addr, render, tc.want)
			}
		})
	}

	var masklen int
	if err := d.QueryRow(`SELECT cidr_masklen('10.0.0.0/17')`).Scan(&masklen); err != nil {
		t.Fatalf("masklen: %v", err)
	}
	if masklen != 17 {
		t.Fatalf("cidr_masklen = %d, want 17", masklen)
	}
}

// 网段归属的端到端：最长前缀优先，脏 IP 跳过且出 warning。
func TestT02NetworkAttributionLongestPrefix(t *testing.T) {
	d, _ := t02Setup(t)
	cs := d.Companies()

	broad, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Broad %d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatalf("company broad: %v", err)
	}
	narrow, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Narrow %d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatalf("company narrow: %v", err)
	}
	if _, _, _, _, err := cs.AddScopeInputsChecked(broad, []ScopeInput{{Kind: "cidr", Value: "10.1.0.0/16"}}, "t02"); err != nil {
		t.Fatalf("scope broad: %v", err)
	}
	if _, _, _, _, err := cs.AddScopeInputsChecked(narrow, []ScopeInput{{Kind: "cidr", Value: "10.1.2.0/24"}}, "t02"); err != nil {
		t.Fatalf("scope narrow: %v", err)
	}

	if _, err := d.Exec(`INSERT INTO assets(type, ip, task_ids) VALUES('ip','10.1.2.3','[]')`); err != nil {
		t.Fatalf("insert ip: %v", err)
	}
	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	var gotCompany int64
	if err := d.QueryRow(`SELECT COALESCE(company_id,0) FROM assets WHERE ip='10.1.2.3'`).Scan(&gotCompany); err != nil {
		t.Fatalf("read attribution: %v", err)
	}
	if gotCompany != narrow {
		t.Fatalf("10.1.2.3 attributed to %d, want narrow %d (longest prefix wins; broad=%d)",
			gotCompany, narrow, broad)
	}

	// 桶外地址归 broad
	if _, err := d.Exec(`INSERT INTO assets(type, ip, task_ids) VALUES('ip','10.1.9.9','[]')`); err != nil {
		t.Fatalf("insert ip2: %v", err)
	}
	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatalf("recompute2: %v", err)
	}
	// 10.1.9.9 在 /16 内但不在 /24 内 → broad
	if err := d.QueryRow(`SELECT COALESCE(company_id,0) FROM assets WHERE ip='10.1.9.9'`).Scan(&gotCompany); err != nil {
		t.Fatalf("read attribution2: %v", err)
	}
	if gotCompany != broad {
		t.Fatalf("10.1.9.9 attributed to %d, want broad %d", gotCompany, broad)
	}
}

// 脏 IP 必须不出现在网段归属里，并且被 malformedIPAssetWarning 报出来。
func TestT02MalformedIPSkippedAndWarned(t *testing.T) {
	d, _ := t02Setup(t)
	cs := d.Companies()
	co, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Malformed %d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	if _, _, _, _, err := cs.AddScopeInputsChecked(co, []ScopeInput{{Kind: "cidr", Value: "10.0.0.0/8"}}, "t02"); err != nil {
		t.Fatalf("scope: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO assets(type, ip, task_ids) VALUES('ip','not-an-ip','[]')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	var companyID *int64
	if err := d.QueryRow(`SELECT company_id FROM assets WHERE ip='not-an-ip'`).Scan(&companyID); err != nil {
		t.Fatalf("read: %v", err)
	}
	if companyID != nil {
		t.Fatalf("malformed ip got attributed to company %d; cidr_contains NULL must filter it out", *companyID)
	}
	warn, err := cs.MalformedIPAssetWarning()
	if err != nil {
		t.Fatalf("warning: %v", err)
	}
	if warn == "" {
		t.Fatal("malformed ip warning is empty; the operator would never learn about this row")
	}
}

// 域名归属：同一 root_domain 被多条规则命中时，取具体度最高的（这里用
// company_id 兜平手，验证 ROW_NUMBER 的 ORDER BY 真的生效而不是随便取一行）。
func TestT02DomainAttributionTieBreak(t *testing.T) {
	d, _ := t02Setup(t)
	cs := d.Companies()
	suffix := time.Now().UnixNano()

	first, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Tie A %d", suffix), "")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Tie B %d", suffix), "")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	// 两家公司都声明同一个 root domain（真实场景：集团与子公司共用主域）。
	for _, co := range []int64{first, second} {
		if _, _, _, _, err := cs.AddScopeInputsChecked(co, []ScopeInput{{Kind: "domain", Value: "t02-tie.example.com"}}, "t02"); err != nil {
			t.Fatalf("scope %d: %v", co, err)
		}
	}
	if _, err := d.Exec(`INSERT INTO assets(type, domain, root_domain, task_ids) VALUES('subdomain','t02-tie.example.com','t02-tie.example.com','[]')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	var got int64
	if err := d.QueryRow(`SELECT COALESCE(company_id,0) FROM assets WHERE domain='t02-tie.example.com'`).Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	want := first
	if second < first {
		want = second
	}
	if got != want {
		t.Fatalf("tie-break attributed to %d, want lowest company id %d (first=%d second=%d)",
			got, want, first, second)
	}
}

// 资产 root_domain 精确等于窄规则的域时，窄规则赢。
func TestT02DomainAttributionLongestDomain(t *testing.T) {
	d, _ := t02Setup(t)
	cs := d.Companies()
	suffix := time.Now().UnixNano()

	parent, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Parent %d", suffix), "")
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	child, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Child %d", suffix), "")
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	if _, _, _, _, err := cs.AddScopeInputsChecked(parent, []ScopeInput{{Kind: "domain", Value: "example.com"}}, "t02"); err != nil {
		t.Fatalf("scope parent: %v", err)
	}
	if _, _, _, _, err := cs.AddScopeInputsChecked(child, []ScopeInput{{Kind: "domain", Value: "api.example.com"}}, "t02"); err != nil {
		t.Fatalf("scope child: %v", err)
	}

	// root_domain 只等于 example.com → 只能命中 parent（与改前的 PG 语义一致）。
	if _, err := d.Exec(`INSERT INTO assets(type, domain, root_domain, task_ids) VALUES('subdomain','api.example.com','example.com','[]')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// root_domain 等于 api.example.com → 只命中 child。
	if _, err := d.Exec(`INSERT INTO assets(type, domain, root_domain, task_ids) VALUES('subdomain','x.api.example.com','api.example.com','[]')`); err != nil {
		t.Fatalf("insert2: %v", err)
	}
	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	var gotRoot, gotSub int64
	if err := d.QueryRow(`SELECT COALESCE(company_id,0) FROM assets WHERE domain='api.example.com'`).Scan(&gotRoot); err != nil {
		t.Fatalf("read root: %v", err)
	}
	if gotRoot != parent {
		t.Fatalf("api.example.com (root=example.com) attributed to %d, want parent %d", gotRoot, parent)
	}
	if err := d.QueryRow(`SELECT COALESCE(company_id,0) FROM assets WHERE domain='x.api.example.com'`).Scan(&gotSub); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if gotSub != child {
		t.Fatalf("x.api.example.com (root=api.example.com) attributed to %d, want child %d", gotSub, child)
	}
}

// ---------------------------------------------------------------------------
// 5. asset_dsl：模糊搜索 / 数组包含 / task_id 展开
// ---------------------------------------------------------------------------

func TestT02DSLFuzzyAndArrayMembership(t *testing.T) {
	d, assets := t02Setup(t)
	taskID := t02NewTask(t, d, "dsl")

	if _, err := assets.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://Shop.T02Example.com/login",
		Technologies: []string{"nginx", "React"},
		PageTitle:    "登录页",
		TaskID:       taskID,
	}); err != nil {
		t.Fatalf("upsert service: %v", err)
	}

	// 大小写不敏感模糊（SQLite LIKE 对 ASCII 已覆盖）
	got, err := assets.QueryDSL(`domain=shop.t02example.com`, "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("domain fuzzy: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("domain fuzzy (lowercase query) = %d rows, want 1", len(got))
	}
	got, err = assets.QueryDSL(`domain=SHOP.T02EXAMPLE.COM`, "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("domain fuzzy upper: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("domain fuzzy (uppercase query) = %d rows, want 1", len(got))
	}

	// 数组元素精确匹配（json_each 替代 = ANY(col)）
	got, err = assets.QueryDSL(`technology==nginx`, "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("tech exact: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("technology==nginx = %d rows, want 1", len(got))
	}
	// 大小写不同必须不匹配（== 是精确比较，不是模糊）
	got, err = assets.QueryDSL(`technology==NGINX`, "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("tech exact upper: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("technology==NGINX = %d rows, want 0 (exact match is case-sensitive)", len(got))
	}

	// 数组内模糊（LIKE 替代 ILIKE + unnest）
	got, err = assets.QueryDSL(`technology=rea`, "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("tech fuzzy: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("technology=rea (fuzzy, matches React) = %d rows, want 1", len(got))
	}

	// 裸词全文：中文标题
	got, err = assets.QueryDSL(`登录`, "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("bare cjk: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("bare 登录 = %d rows, want 1", len(got))
	}

	// task_id 走 json_each 展开
	got, err = assets.QueryDSL(fmt.Sprintf(`task_id==%d`, taskID), "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("task_id dsl: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("task_id==%d = %d rows, want 1", taskID, len(got))
	}
	// 不存在的 task_id 必须返回 0 而不是报错
	got, err = assets.QueryDSL(fmt.Sprintf(`task_id==%d`, taskID+999999), "service", 0, 10, 0)
	if err != nil {
		t.Fatalf("task_id miss: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("missing task_id = %d rows, want 0", len(got))
	}

	// CountDSL 与 QueryDSL 的 WHERE 必须同源
	if n, err := assets.CountDSL(`technology==nginx`, "service", 0); err != nil || n != 1 {
		t.Fatalf("CountDSL = %d, err=%v", n, err)
	}
	if n, err := assets.CountDSL(`technology==nginx`, "service", taskID); err != nil || n != 1 {
		t.Fatalf("CountDSL(task) = %d, err=%v", n, err)
	}
	if n, err := assets.CountDSL(`technology==nginx`, "service", taskID+999999); err != nil || n != 0 {
		t.Fatalf("CountDSL(other task) = %d, err=%v", n, err)
	}
}

// ---------------------------------------------------------------------------
// 6. 分页 / 排序（pageClause 是运行时拼 $N，最容易跳号）
// ---------------------------------------------------------------------------

func TestT02PaginationPlaceholdersAreContiguous(t *testing.T) {
	d, assets := t02Setup(t)
	taskID := t02NewTask(t, d, "paging")
	for i := 0; i < 5; i++ {
		if _, err := assets.UpsertRootDomain(UpsertRootDomainReq{
			Domain: fmt.Sprintf("t02-page-%d.example.com", i), TaskID: taskID,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// 无 type 过滤 / 有 type 过滤 × 有 limit / 有 limit+offset：四种拼法都要能跑
	if rows, err := assets.QueryByCompany(0, "", 2, 0); err != nil {
		t.Fatalf("company no typ: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("company 0 should have 0 rows, got %d", len(rows))
	}
	for _, typ := range []string{"", "root_domain"} {
		for _, page := range [][2]int{{0, 0}, {2, 0}, {2, 2}} {
			got, err := assets.QueryByTask(taskID, typ, page[0], page[1])
			if err != nil {
				t.Fatalf("QueryByTask typ=%q limit=%d offset=%d: %v", typ, page[0], page[1], err)
			}
			want := 5
			if page[0] > 0 {
				if page[1] >= 5 {
					want = 0
				} else if page[1]+page[0] > 5 {
					want = 5 - page[1]
				} else {
					want = page[0]
				}
			}
			if len(got) != want {
				t.Fatalf("QueryByTask typ=%q limit=%d offset=%d = %d rows, want %d",
					typ, page[0], page[1], len(got), want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 7. task_scope 幂等（ON CONFLICT 表达式目标）
// ---------------------------------------------------------------------------

func TestT02TaskScopeUpsertIdempotent(t *testing.T) {
	d, assets := t02Setup(t)
	taskID := t02NewTask(t, d, "scope-idempotent")

	for i := 0; i < 3; i++ {
		ok, err := assets.upsertTaskScopeResult(TaskScope{
			TaskID: taskID, Kind: "ip", Net: "10.5.5.5/32",
		})
		if err != nil {
			t.Fatalf("upsert #%d: %v", i, err)
		}
		if i == 0 && !ok {
			t.Fatal("first insert reported not-inserted")
		}
		if i > 0 && ok {
			t.Fatalf("duplicate insert #%d reported inserted; ON CONFLICT target must dedupe", i)
		}
	}
	scopes, err := assets.ListTaskScope(taskID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(scopes) != 1 {
		t.Fatalf("want 1 scope row after 3 identical inserts, got %d", len(scopes))
	}
	if scopes[0].Net != "10.5.5.5/32" {
		t.Fatalf("net not round-tripped: %q", scopes[0].Net)
	}
}

// ---------------------------------------------------------------------------
// 8. company_scope 幂等（ON CONFLICT ON CONSTRAINT 改写）
// ---------------------------------------------------------------------------

func TestT02CompanyScopeUpsertIdempotent(t *testing.T) {
	d, _ := t02Setup(t)
	cs := d.Companies()
	co, _, err := cs.UpsertCompany(fmt.Sprintf("T02 Scope Dedupe %d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	added, skipped, invalid, verrs, err := cs.AddScopeInputsChecked(co, []ScopeInput{
		{Kind: "domain", Value: "t02-scope.example.com"},
		{Kind: "cidr", Value: "192.168.10.0/24"},
		{Kind: "icp", Value: "京ICP备00000001号-1"},
	}, "t02")
	if err != nil {
		t.Fatalf("add: %v (invalid=%d verrs=%v)", err, invalid, verrs)
	}
	if added != 3 || skipped != 0 {
		t.Fatalf("first add: added=%d skipped=%d, want 3/0", added, skipped)
	}
	added2, skipped2, _, _, err := cs.AddScopeInputsChecked(co, []ScopeInput{
		{Kind: "domain", Value: "t02-scope.example.com"},
		{Kind: "cidr", Value: "192.168.10.0/24"},
		{Kind: "icp", Value: "京ICP备00000001号-1"},
	}, "t02")
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if added2 != 0 || skipped2 != 3 {
		t.Fatalf("re-add: added=%d skipped=%d, want 0/3 (DO NOTHING must not insert)", added2, skipped2)
	}
}

// ---------------------------------------------------------------------------
// 9. exec_accounts market_support 往返（TEXT 列 + '{}' 默认值兼容）
// ---------------------------------------------------------------------------

func TestT02ExecAccountMarketSupportRoundTrip(t *testing.T) {
	d, _ := t02Setup(t)
	store := d.ExecAccounts()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	acct, err := store.UpsertAccount(ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: "t02-" + suffix,
		Label:         "T02",
		Market:        "crypto",
		MarketSupport: []string{"crypto"},
		Active:        true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if len(acct.MarketSupport) != 1 || acct.MarketSupport[0] != "crypto" {
		t.Fatalf("market_support round-trip = %v", acct.MarketSupport)
	}
	if acct.BlockedAt != nil {
		t.Fatalf("new account must not be blocked, got %v", *acct.BlockedAt)
	}
	if err := store.Block(acct.ID, "t02 risk"); err != nil {
		t.Fatalf("block: %v", err)
	}
	blocked, err := store.GetAccount(acct.ID)
	if err != nil {
		t.Fatalf("get blocked: %v", err)
	}
	if blocked.BlockedAt == nil || blocked.BlockReason != "t02 risk" {
		t.Fatalf("blocked_at=%v reason=%q, want set/'t02 risk'", blocked.BlockedAt, blocked.BlockReason)
	}
}

// ---------------------------------------------------------------------------
// 10. snapshots 去重键（date_trunc → substr(collected_at,1,19)）
// ---------------------------------------------------------------------------

func TestT02SnapshotDedupTruncatesToSecond(t *testing.T) {
	d, _ := t02Setup(t)
	store := d.Snapshots()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	in := SnapshotInput{
		Source:      "t02-" + suffix,
		Endpoint:    "/quotes",
		ParamsHash:  "h1",
		CollectedAt: "2026-01-02T03:04:05.100Z",
		Checksum:    "c1",
	}
	snap, created, err := store.InsertSnapshot(in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !created {
		t.Fatal("first insert must be created=true")
	}
	// 同一秒、不同毫秒 → 命中同一条（date_trunc('second') 语义）
	in2 := in
	in2.CollectedAt = "2026-01-02T03:04:05.900Z"
	in2.Checksum = in.Checksum
	snap2, created2, err := store.InsertSnapshot(in2)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if created2 {
		t.Fatal("same-second insert must dedupe (created=false)")
	}
	if snap2.ID != snap.ID {
		t.Fatalf("dedup returned id %d, want %d", snap2.ID, snap.ID)
	}
	inConflict := in2
	inConflict.Checksum = "different-content"
	if got, created, err := store.InsertSnapshot(inConflict); !errors.Is(err, ErrSnapshotConflict) || got != nil || created {
		t.Fatalf("same-second different content must conflict: snap=%+v created=%v err=%v", got, created, err)
	}
	// 下一秒 → 新行
	in3 := in
	in3.CollectedAt = "2026-01-02T03:04:06.000Z"
	if _, created3, err := store.InsertSnapshot(in3); err != nil {
		t.Fatalf("third: %v", err)
	} else if !created3 {
		t.Fatal("next-second insert must be a new row")
	}
	if ids, err := store.ExistingIDs([]int64{snap.ID, snap.ID + 1<<40}); err != nil {
		t.Fatalf("ExistingIDs: %v", err)
	} else if !ids[snap.ID] {
		t.Fatalf("ExistingIDs missing %d: %v", snap.ID, ids)
	}
}
