import { isRoleKey, parseIntents, roleText } from "./intent-label.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("payload 带 role 时同时解出摘要与角色", () => {
  const got = parseIntents('{"summary":"综合三方风控出评级","role":"portfolio_manager"}');
  assert.deepEqual(got, { summary: "综合三方风控出评级", role: "portfolio_manager" });
});

test("没有 role 的渗透 intent 角色为空", () => {
  const got = parseIntents('{"summary":"test idor"}');
  assert.deepEqual(got, { summary: "test idor", role: "" });
});

test("非法 JSON 与非 JSON 文本回退原文，不抛", () => {
  assert.deepEqual(parseIntents("{坏掉的"), { summary: "{坏掉的", role: "" });
  assert.deepEqual(parseIntents("纯文本方向"), { summary: "纯文本方向", role: "" });
  assert.deepEqual(parseIntents(""), { summary: "", role: "" });
  assert.deepEqual(parseIntents(undefined), { summary: "", role: "" });
});

test("summary 缺失时回退原文（不显示空标题）", () => {
  assert.deepEqual(parseIntents('{"role":"trader"}').summary, '{"role":"trader"}');
});

test("work#N 槽名不算角色", () => {
  assert.equal(isRoleKey("work#3"), false);
  assert.equal(isRoleKey("WORK#12"), false);
  assert.equal(isRoleKey(""), false);
  assert.equal(isRoleKey("  "), false);
  assert.equal(isRoleKey("analyst_market"), true);
});

test("roleText 有中文名时拼「中文名 key」，否则只给 key", () => {
  const names = new Map([["analyst_market", "技术面分析师"]]);
  assert.equal(roleText("analyst_market", names), "技术面分析师 analyst_market");
  assert.equal(roleText("trader", names), "trader");
  assert.equal(roleText("work#1", names), "");
});
