const api = globalThis.browser ?? globalThis.chrome;

const $ = (id) => document.getElementById(id);

function render(r) {
  const items = $("items");
  items.textContent = "";
  if (!r) {
    $("summary").textContent = "还没同步过";
    return;
  }
  const when = new Date(r.at).toLocaleString();
  $("summary").textContent = r.error ? `${when} 同步失败：${r.error}` : `${when}（${r.reason}）`;
  for (const i of r.items || []) {
    const li = document.createElement("li");
    li.className = i.ok ? "ok" : "fail";
    li.textContent = `${i.label || "未命名"} /u/${i.authuser}：${i.message}`;
    items.appendChild(li);
  }
  if (r.warning) {
    const li = document.createElement("li");
    li.className = "fail";
    li.textContent = r.warning;
    items.appendChild(li);
  }
}

$("syncNow").addEventListener("click", async () => {
  $("syncNow").disabled = true;
  $("summary").textContent = "同步中…";
  try {
    render(await api.runtime.sendMessage({ type: "sync-now" }));
  } finally {
    $("syncNow").disabled = false;
  }
});
$("openOptions").addEventListener("click", () => api.runtime.openOptionsPage());

api.storage.local.get("lastResult").then((c) => render(c.lastResult));
