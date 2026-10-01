const api = async (path, options = {}) => {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(body.error || `Request failed (${response.status})`);
    error.status = response.status;
    error.body = body;
    throw error;
  }
  return body;
};

const text = (value) => value === undefined || value === null || value === "" ? "-" : String(value);
const shortID = (value) => value ? String(value).slice(0, 8) : "-";
const setMessage = (element, value) => { if (element) element.textContent = value || ""; };
const withBusy = async (button, action) => {
  if (button) button.disabled = true;
  try { return await action(); } finally { if (button) button.disabled = false; }
};

window.LockFlow = { api, text, shortID, setMessage, withBusy };
