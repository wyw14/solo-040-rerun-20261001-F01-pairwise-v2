const { api, shortID, setMessage, withBusy } = window.LockFlow;
const form = document.querySelector("#plan-form");
const message = document.querySelector("#plan-message");
const plansHost = document.querySelector("#plans");

// drafts[planID] = { rows: [{ id, name, callSign, lengthM }], dirty: bool }
const drafts = {};
let latestViews = {};

const escapeHTML = (value) => String(value ?? "").replace(/[&<>"']/g, (ch) => (
  { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]
));

function seedDraft(view) {
  const existing = drafts[view.plan.id];
  if (existing && existing.dirty) return;
  drafts[view.plan.id] = {
    dirty: false,
    rows: view.plan.vessels.map((v) => ({ id: v.id, name: v.name, callSign: v.call_sign, lengthM: v.length_m })),
  };
}

function renderPlans(plansByChamber) {
  const views = Object.entries(plansByChamber).flatMap(([chamber, items]) =>
    items.map((view) => ({ ...view, chamber })));
  latestViews = {};
  views.forEach((view) => { latestViews[view.plan.id] = view; seedDraft(view); });

  if (!views.length) {
    plansHost.innerHTML = '<div class="muted">No active plans</div>';
    return;
  }
  plansHost.innerHTML = views.map(renderCard).join("");
}

function renderCard(view) {
  const plan = view.plan;
  const draft = drafts[plan.id] || { rows: [] };
  const stage = view.bound_cycle
    ? `<span class="badge stage">${escapeHTML(view.stage || "planned")}</span>`
    : `<span class="badge stage">waiting</span>`;
  const stateBadge = view.editable
    ? `<span class="badge">editable</span>`
    : `<span class="badge locked-badge">locked</span>`;
  const reject = view.editable ? "" : `<div class="reject">修订被拒绝：${escapeHTML(view.reject_reason)}</div>`;

  const rows = draft.rows.map((row, index) => {
    const up = `<button class="iconbtn" data-action="up" data-index="${index}" ${index === 0 ? "disabled" : ""}>↑</button>`;
    const down = `<button class="iconbtn" data-action="down" data-index="${index}" ${index === draft.rows.length - 1 ? "disabled" : ""}>↓</button>`;
    const detail = row.id
      ? `<span><span class="vname">${escapeHTML(row.name)}</span> <span class="vsub">${escapeHTML(row.callSign)} · ${row.lengthM}m</span></span><span class="vsub">${shortID(row.id)}</span>`
      : `<span><span class="vname">${escapeHTML(row.name || "(new vessel)")}</span> <span class="vsub">${escapeHTML(row.callSign || "—")} · ${row.lengthM || 0}m</span></span><span class="vsub">new</span>`;
    return `<div class="vessel-row"><span class="muted">${index + 1}</span>${detail}<span></span><span>${up} ${down}</span><span><button class="danger" data-action="remove" data-index="${index}">删除</button></span></div>`;
  }).join("");

  const addRow = view.editable ? `
    <div class="add-row">
      <input data-field="name" placeholder="New vessel name">
      <input data-field="callSign" placeholder="Call sign">
      <input data-field="lengthM" type="number" min="1" placeholder="Length m">
      <button data-action="add">＋ 增加船舶</button>
    </div>` : "";

  const actions = view.editable ? `
    <div class="plan-actions">
      <button data-action="save" data-plan="${plan.id}">保存修订</button>
      <button class="danger" data-action="cancel" data-plan="${plan.id}">取消计划</button>
    </div>` : `
    <div class="plan-actions">
      <button class="danger" data-action="cancel" data-plan="${plan.id}">取消计划</button>
    </div>`;

  return `<section class="plan-card ${view.editable ? "" : "locked"}" data-plan="${plan.id}">
    <div class="plan-head">
      <strong>${escapeHTML(view.chamber)}</strong>
      <span class="muted">plan ${shortID(plan.id)} · ${escapeHTML(plan.direction)}</span>
      <span class="badge">v${view.current_version}</span>
      <span class="muted">${view.revisions} version(s)</span>
      ${stage}${stateBadge}
      <span class="spacer"></span>
      <span class="muted">created ${new Date(plan.created_at).toLocaleTimeString()}</span>
    </div>
    ${reject}
    <div class="vessel-rows">${rows}</div>
    ${addRow}
    ${actions}
  </section>`;
}

function markDirty(planID) {
  if (drafts[planID]) drafts[planID].dirty = true;
}

async function refresh() {
  const [plans, chambers, health] = await Promise.all([api("/api/plans"), api("/api/chambers"), api("/healthz")]);
  document.querySelector("#health").textContent = health.status === "ok" ? "Devices online" : "Attention required";
  renderPlans(plans);
  const current = chambers.find((item) => item.cycle);
  document.querySelector("#metric-chamber").textContent = current?.chamber_id || "Idle";
  document.querySelector("#metric-stage").textContent = current?.cycle?.stage || "-";
  document.querySelector("#metric-cycle").textContent = shortID(current?.cycle?.id);
}

plansHost.addEventListener("click", async (event) => {
  const button = event.target.closest("button");
  if (!button || button.disabled) return;
  const action = button.dataset.action;
  const card = button.closest(".plan-card");
  const planID = card?.dataset.plan || button.dataset.plan;
  const draft = drafts[planID];

  if (action === "up" || action === "down") {
    const index = Number(button.dataset.index);
    const swap = action === "up" ? index - 1 : index + 1;
    if (swap < 0 || swap >= draft.rows.length) return;
    [draft.rows[index], draft.rows[swap]] = [draft.rows[swap], draft.rows[index]];
    markDirty(planID);
    renderPlans(latestViewsAsPlans());
    return;
  }
  if (action === "remove") {
    draft.rows.splice(Number(button.dataset.index), 1);
    markDirty(planID);
    renderPlans(latestViewsAsPlans());
    return;
  }
  if (action === "add") {
    const fields = Object.fromEntries([...card.querySelectorAll(".add-row [data-field]")].map((el) => [el.dataset.field, el.value]));
    const lengthM = Number(fields.lengthM);
    if (!fields.name || !fields.callSign || lengthM <= 0) {
      setMessage(message, "新船需要名称、呼号和正的长度。");
      return;
    }
    draft.rows.push({ id: null, name: fields.name, callSign: fields.callSign, lengthM });
    markDirty(planID);
    renderPlans(latestViewsAsPlans());
    return;
  }
  if (action === "save") {
    await withBusy(button, () => saveRevision(planID, button));
    return;
  }
  if (action === "cancel") {
    await withBusy(button, () => cancelPlan(planID, button));
  }
});

// Rebuild the plans-by-chamber structure from the latest fetched views.
function latestViewsAsPlans() {
  const grouped = {};
  Object.values(latestViews).forEach((view) => {
    (grouped[view.chamber] ||= []).push(view);
  });
  return grouped;
}

async function saveRevision(planID, button) {
  const draft = drafts[planID];
  const view = latestViews[planID];
  if (!draft.rows.length) {
    setMessage(message, "修订后至少要保留一艘船。");
    return;
  }
  const payload = {
    expected_version: view.current_version,
    vessels: draft.rows.map((row) => row.id
      ? { vessel_id: row.id }
      : { name: row.name, call_sign: row.callSign, length_m: row.lengthM }),
  };
  try {
    const result = await api(`/api/plans/${planID}/revise`, { method: "POST", body: JSON.stringify(payload) });
    delete drafts[planID];
    setMessage(message, `计划 ${shortID(planID)} 已修订为 v${result.version}（队列 ${result.queue_length} 艘）。`);
    await refresh();
  } catch (error) {
    if (error.status === 409) {
      const current = error.body?.current_version ? `（当前 v${error.body.current_version}）` : "";
      setMessage(message, `修订被拒绝${current}：${error.message}`);
      // A conflict means the server copy moved on; drop the stale draft.
      delete drafts[planID];
      await refresh().catch(() => {});
    } else {
      setMessage(message, error.message);
    }
  }
}

async function cancelPlan(planID) {
  try {
    await api(`/api/plans/${planID}/cancel`, { method: "POST", body: "{}" });
    delete drafts[planID];
    setMessage(message, `计划 ${shortID(planID)} 已取消。`);
    await refresh();
  } catch (error) {
    setMessage(message, error.message);
  }
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = form.querySelector("button");
  await withBusy(button, async () => {
    try {
      const result = await api("/api/plans", { method: "POST", body: JSON.stringify({
        chamber_id: document.querySelector("#chamber").value,
        direction: document.querySelector("#direction").value,
        vessels: [{ name: document.querySelector("#vessel").value, call_sign: document.querySelector("#call-sign").value, length_m: Number(document.querySelector("#length").value) }],
      }) });
      setMessage(message, result.queued
        ? `Plan ${shortID(result.plan.id)} queued in ${result.plan.chamber_id} (transit busy).`
        : `Plan ${shortID(result.plan.id)} created in ${result.plan.chamber_id}.`);
      await refresh();
    } catch (error) { setMessage(message, error.message); }
  });
});

let timer;
async function loop() {
  try { await refresh(); } catch (error) { setMessage(message, error.message); }
  timer = setTimeout(loop, 5000);
}
// Pause polling while any plan has unsaved edits so drafts are not overwritten.
setInterval(() => {
  const dirty = Object.values(drafts).some((draft) => draft.dirty);
  if (dirty && timer) { clearTimeout(timer); timer = null; }
  if (!dirty && !timer) { timer = setTimeout(loop, 1000); }
}, 1000);

loop();
