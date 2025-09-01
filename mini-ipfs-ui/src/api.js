// Defaults match docker-compose public ports:
// node1 -> 8081, node2 -> 8082, node3 -> 8083
// Override via Vite envs if needed
const N1 = import.meta.env.VITE_NODE1_BASE || "http://localhost:8081";
const N2 = import.meta.env.VITE_NODE2_BASE || "http://localhost:8082";
const N3 = import.meta.env.VITE_NODE3_BASE || "http://localhost:8083";
export const nodes = { node1: N1, node2: N2, node3: N3 };

export const getHealth = (base) =>
  fetch(base + "/health").then((r) => r.json());

export const getStatus = (base) =>
  fetch(base + "/api/v1/status").then((r) => r.json());

export const uploadFile = async (base, file, stream = true) => {
  const path = stream ? "/api/v1/file/stream" : "/api/v1/file";
  const res = await fetch(base + path, {
    method: "POST",
    body: file,
  });
  if (!res.ok) throw new Error(`Upload failed: ${res.status}`);
  return res.json(); // { hash, size, chunks, manifest_size }
};

export const downloadURL = (base, manifestHash, remote = true) =>
  `${base}/api/v1/file/${manifestHash}${remote ? "?remote=1" : ""}`;

export const deleteFile = async (base, manifestHash) => {
  const res = await fetch(`${base}/api/v1/file/${manifestHash}?remote=1&global=1`, { method: "DELETE" });
  if (!res.ok) throw new Error(`Delete failed: ${res.status}`);
  return res.json();
};

export const connectEvents = (base, onEvent) => {
  const es = new EventSource(`${base}/api/v1/events`);
  es.onmessage = (e) => {
    try {
      const data = JSON.parse(e.data);
      onEvent?.(data);
    } catch {}
  };
  es.onopen = () => {
    onEvent?.({ event: 'sse_open', ts: Date.now() });
  };
  es.onerror = () => {
    onEvent?.({ event: 'sse_error', ts: Date.now() });
    // EventSource will attempt to reconnect automatically
  };
  return es;
};
