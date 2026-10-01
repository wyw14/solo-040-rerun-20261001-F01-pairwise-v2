const { api, shortID, setMessage, withBusy } = window.LockFlow;
const form = document.querySelector("#plan-form");
const message = document.querySelector("#plan-message");
const editPanel = document.querySelector("#edit-panel");
const editMessage = document.querySelector("#edit-message");
const editBody = document.querySelector("#edit-vessels");

let plansCache = [];
let editing = null;

async function refresh() {
  const [plans, chambers, health] = await Promise.all([api("/api/plans"), api("/api/chambers"), api("/healthz")]);
  document.querySelector("#health").textContent = health.status === "ok" ? "Devices online" : "Attention required";
  plansCache = Object.entries(plans).flatMap(([chamber, items]) => items.map((plan) => ({ ...plan, chamber })));
  const rows = plansCache.map((plan) => `<tr><td>${plan.chamber}</td><td>${shortID(plan.id)}</td><td>v${plan.version}</td><td>${plan.direction}</td><td>${plan.vessels.length}</td><td>${new Date(plan.created_at).toLocaleTimeString()}</td><td><button type="button" data-edit="${plan.id}">Edit</button></td></tr>`);
  document.querySelector("#plans").innerHTML = rows.join("") || '<tr><td colspan="7" class="muted">No active plans</td></tr>';
  const current = chambers.find((item) => item.cycle);
  document.querySelector("#metric-chamber").textContent = current?.chamber_id || "Idle";
  document.querySelector("#metric-stage").textContent = current?.cycle?.stage || "-";
  document.querySelector("#metric-cycle").textContent = shortID(current?.cycle?.id);
  if (editing) {
    const latest = plansCache.find((plan) => plan.id === editing.id);
    if (!latest) { closeEditor(); return; }
    syncVersion(latest);
  }
}

function syncVersion(latest) {
  editing.version = latest.version;
  document.querySelector("#edit-version").textContent = `v${latest.version}`;
}

function closeEditor() {
  editing = null;
  editPanel.hidden = true;
  setMessage(editMessage, "");
}

function openEditor(plan) {
  editing = {
    id: plan.id,
    version: plan.version,
    vessels: plan.vessels.map((vessel) => ({ ...vessel })),
  };
  editPanel.hidden = false;
  document.querySelector("#edit-plan-id").textContent = shortID(plan.id);
  syncVersion(plan);
  setMessage(editMessage, "");
  renderVessels();
}

function renderVessels() {
  const rows = editing.vessels.map((vessel, index) => `<tr>
    <td>${index + 1}</td><td>${vessel.name || ""}</td><td>${vessel.call_sign || ""}</td><td>${vessel.length_m || ""}</td>
    <td><button type="button" data-move="up" data-index="${index}" ${index === 0 ? "disabled" : ""}>↑</button>
        <button type="button" data-move="down" data-index="${index}" ${index === editing.vessels.length - 1 ? "disabled" : ""}>↓</button></td>
    <td><button type="button" class="danger" data-remove="${index}">Remove</button></td>
  </tr>`);
  editBody.innerHTML = rows.join("") || '<tr><td colspan="6" class="muted">No vessels — add at least one.</td></tr>';
}

document.querySelector("#plans").addEventListener("click", (event) => {
  const id = event.target.getAttribute("data-edit");
  if (!id) return;
  const plan = plansCache.find((item) => item.id === id);
  if (plan) openEditor(plan);
});

editBody.addEventListener("click", (event) => {
  if (!editing) return;
  const removeIndex = event.target.getAttribute("data-remove");
  const move = event.target.getAttribute("data-move");
  if (removeIndex !== null) {
    editing.vessels.splice(Number(removeIndex), 1);
    renderVessels();
  } else if (move) {
    const index = Number(event.target.getAttribute("data-index"));
    const target = move === "up" ? index - 1 : index + 1;
    if (target >= 0 && target < editing.vessels.length) {
      const [item] = editing.vessels.splice(index, 1);
      editing.vessels.splice(target, 0, item);
      renderVessels();
    }
  }
});

document.querySelector("#add-vessel-btn").addEventListener("click", () => {
  const name = document.querySelector("#add-vessel").value.trim();
  const callSign = document.querySelector("#add-callsign").value.trim();
  const length = Number(document.querySelector("#add-length").value);
  if (!editing) return;
  if (!name || !callSign || length <= 0) {
    setMessage(editMessage, "Provide a vessel name, call sign and positive length.");
    return;
  }
  editing.vessels.push({ name, call_sign: callSign, length_m: length });
  document.querySelector("#add-vessel").value = "";
  document.querySelector("#add-callsign").value = "";
  renderVessels();
  setMessage(editMessage, "");
});

document.querySelector("#edit-cancel").addEventListener("click", closeEditor);

document.querySelector("#edit-save").addEventListener("click", async (event) => {
  if (!editing) return;
  if (editing.vessels.length === 0) {
    setMessage(editMessage, "A plan must keep at least one vessel.");
    return;
  }
  await withBusy(event.target, async () => {
    try {
      const result = await api(`/api/plans/${editing.id}/revisions`, {
        method: "POST",
        body: JSON.stringify({
          expected_version: editing.version,
          note: document.querySelector("#edit-note").value.trim(),
          vessels: editing.vessels.map((vessel) => vessel.id
            ? { id: vessel.id }
            : { name: vessel.name, call_sign: vessel.call_sign, length_m: vessel.length_m }),
        }),
      });
      setMessage(editMessage, `Saved v${result.from_version} → v${result.to_version}.`);
      await refresh();
      const updated = plansCache.find((plan) => plan.id === editing.id);
      if (updated) {
        openEditor(updated);
        setMessage(editMessage, `Saved v${result.from_version} → v${result.to_version}.`);
      }
    } catch (error) {
      const body = error.body || {};
      const current = body.version ? `Current version is v${body.version}.` : "";
      const stage = body.stage ? ` Transit stage: ${body.stage}.` : "";
      setMessage(editMessage, `Revision rejected. ${error.message} ${current}${stage}`);
      await refresh();
    }
  });
});

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
      setMessage(message, `Plan ${shortID(result.plan.id)} v${result.plan.version} admitted to ${result.plan.chamber_id}.`);
      await refresh();
    } catch (error) { setMessage(message, error.message); }
  });
});

refresh().catch((error) => setMessage(message, error.message));
setInterval(() => refresh().catch(() => {}), 5000);
