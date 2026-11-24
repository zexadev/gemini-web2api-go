const api = globalThis.browser ?? globalThis.chrome;

const $ = (id) => document.getElementById(id);

function addRow(acc = { label: "", authuser: 0 }) {
  const tr = document.createElement("tr");
  const label = document.createElement("input");
  label.className = "label";
  label.value = acc.label || "";
  label.placeholder = "例如 Firefox 主号";
  const authuser = document.createElement("input");
  authuser.className = "authuser";
  authuser.type = "number";
  authuser.min = "0";
  authuser.max = "9";
  authuser.value = Number(acc.authuser) || 0;
  const del = document.createElement("button");
  del.type = "button";
  del.textContent = "删除";
  del.addEventListener("click", () => tr.remove());
  for (const el of [label, authuser, del]) {
    const td = document.createElement("td");
    td.appendChild(el);
    tr.appendChild(td);
  }
  $("accounts").appendChild(tr);
}

function updateServerWarn() {
  let warn = false;
  try {
    const u = new URL($("server").value);
    warn = u.protocol === "http:" && !["localhost", "127.0.0.1", "[::1]"].includes(u.hostname);
  } catch (_) {}
  $("serverWarn").hidden = !warn;
}

async function load() {
  const c = await api.storage.local.get(["server", "token", "intervalMin", "accounts"]);
  $("server").value = c.server || "";
  $("token").value = c.token || "";
  $("intervalMin").value = c.intervalMin || 10;
  const accounts = c.accounts && c.accounts.length ? c.accounts : [{ label: "", authuser: 0 }];
  accounts.forEach(addRow);
  updateServerWarn();
}

async function save() {
  const status = $("status");
  status.textContent = "";
  let origin, server;
  try {
    const u = new URL($("server").value.trim());
    if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error();
    origin = u.origin;
    // 保留路径：服务部署在反代子路径下时，接口要拼在路径后面
    server = (u.origin + u.pathname).replace(/\/+$/, "");
  } catch (_) {
    status.textContent = "服务器地址格式不对";
    return;
  }
  // 只申请这一个服务器的访问权限；不给就推不上去。
  const granted = await api.permissions.request({ origins: [origin + "/*"] });
  if (!granted) {
    status.textContent = "没有授予访问该服务器的权限，无法同步";
    return;
  }
  const accounts = [...$("accounts").querySelectorAll("tr")].map((tr) => ({
    label: tr.querySelector(".label").value.trim(),
    authuser: Number(tr.querySelector(".authuser").value) || 0,
  }));
  if (!accounts.length) accounts.push({ label: "", authuser: 0 });
  await api.storage.local.set({
    server,
    token: $("token").value.trim(),
    intervalMin: Math.max(1, Number($("intervalMin").value) || 10),
    accounts,
  });
  status.textContent = "已保存，正在同步…";
  const r = await api.runtime.sendMessage({ type: "sync-now" });
  if (r.error) status.textContent = "已保存，同步失败：" + r.error;
  else status.textContent = "已保存。" + r.items.map((i) => (i.ok ? "✓ " : "✗ ") + i.message).join("；");
}

$("addAccount").addEventListener("click", () => addRow());
$("save").addEventListener("click", save);
$("server").addEventListener("input", updateServerWarn);
load();
