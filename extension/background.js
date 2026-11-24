// 把浏览器当前的 gemini.google.com cookie 推给 gemini-web2api-go 的 /admin/api/cookies/sync。
//
// 浏览器开着时它自己在轮转 __Secure-1PSIDTS（Chrome 的 DBSC 也是浏览器内部做的），
// 所以「定时读浏览器当前的 cookie 推上去」对 Firefox 和 Chrome 导出的号都有效。
// 只发往用户在设置页填的服务器地址，不连任何别的地址。

const api = globalThis.browser ?? globalThis.chrome;

const GEMINI_URL = "https://gemini.google.com/";
const SYNC_PATH = "/admin/api/cookies/sync";
const ALARM_PERIODIC = "periodic-sync";
const ALARM_SOON = "sync-soon";
// 这几项变了说明登录态轮转过，尽快推一次。
const WATCHED = new Set(["__Secure-1PSIDTS", "__Secure-1PSID", "OSID", "SAPISID"]);

const DEFAULTS = {
  server: "",
  token: "",
  intervalMin: 10,
  accounts: [{ label: "", authuser: 0 }],
};

async function loadConfig() {
  const c = await api.storage.local.get(Object.keys(DEFAULTS));
  return { ...DEFAULTS, ...c };
}

function syncURL(server) {
  return server.replace(/\/+$/, "") + SYNC_PATH;
}

// 读 gemini.google.com 这个地址会带上的全部 cookie（含 .google.com 域的和 OSID 这种主机专属的）。
// 以 JSON 数组发，服务端按 domain 再过滤一遍，同名时取更具体的域。
async function readGeminiCookies() {
  const list = await api.cookies.getAll({ url: GEMINI_URL });
  return list.map((c) => ({ name: c.name, value: c.value, domain: c.domain, path: c.path }));
}

async function syncOnce(reason) {
  const cfg = await loadConfig();
  const startedAt = Date.now();
  const result = { at: startedAt, reason, items: [] };
  try {
    if (!cfg.server) throw new Error("还没填服务器地址");
    const cookies = await readGeminiCookies();
    const names = new Set(cookies.map((c) => c.name));
    if (!names.has("__Secure-1PSID")) throw new Error("浏览器里没有 Google 登录 cookie，请先登录 gemini.google.com");
    const missingOSID = !names.has("OSID");
    for (const acc of cfg.accounts) {
      const item = { label: acc.label, authuser: acc.authuser };
      try {
        const resp = await fetch(syncURL(cfg.server), {
          method: "POST",
          headers: cfg.token
            ? { "Content-Type": "application/json", Authorization: "Bearer " + cfg.token }
            : { "Content-Type": "application/json" },
          body: JSON.stringify({ label: acc.label, authuser: Number(acc.authuser) || 0, cookie: cookies }),
          credentials: "omit",
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok || data.success === false) {
          throw new Error(data.error || "HTTP " + resp.status);
        }
        item.ok = true;
        item.message = data.message || data.action;
      } catch (e) {
        item.ok = false;
        item.message = String(e.message || e);
      }
      result.items.push(item);
    }
    if (missingOSID) result.warning = "缺 OSID：先在浏览器里打开一次 gemini.google.com";
  } catch (e) {
    result.error = String(e.message || e);
  }
  await api.storage.local.set({ lastResult: result });
  return result;
}

async function schedule() {
  const cfg = await loadConfig();
  const period = Math.max(1, Number(cfg.intervalMin) || DEFAULTS.intervalMin);
  await api.alarms.clear(ALARM_PERIODIC);
  api.alarms.create(ALARM_PERIODIC, { delayInMinutes: 0.5, periodInMinutes: period });
}

api.runtime.onInstalled.addListener(schedule);
api.runtime.onStartup.addListener(schedule);

api.storage.onChanged.addListener((changes, area) => {
  if (area === "local" && (changes.intervalMin || changes.server)) schedule();
});

api.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === ALARM_PERIODIC || alarm.name === ALARM_SOON) {
    syncOnce(alarm.name === ALARM_SOON ? "cookie 变化" : "定时");
  }
});

// cookie 一轮转可能连着变好几项，用一个 30 秒后的闹钟合并成一次推送。
api.cookies.onChanged.addListener(({ cookie, removed }) => {
  if (removed || !WATCHED.has(cookie.name)) return;
  if (!cookie.domain.replace(/^\./, "").endsWith("google.com")) return;
  api.alarms.create(ALARM_SOON, { delayInMinutes: 0.5 });
});

api.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg && msg.type === "sync-now") {
    syncOnce("手动").then(sendResponse);
    return true;
  }
  return false;
});
