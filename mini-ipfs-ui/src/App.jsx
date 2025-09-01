import React, { useEffect, useRef, useState } from "react";
import { Activity, Download, Eye, FileText, Network, Server, Upload, Zap } from "lucide-react";
import { nodes as nodeBases, getHealth, getStatus, uploadFile, downloadURL, deleteFile, connectEvents } from "./api";

export default function MiniIPFSDashboard() {
  const [selectedNode, setSelectedNode] = useState("node1");
  const [networkActivity, setNetworkActivity] = useState([]);
  const [activitiesByNode, setActivitiesByNode] = useState({}); // { nodeId: [{id, ts, text}] }
  const [sseStatus, setSSEStatus] = useState({}); // { nodeId: 'connecting'|'open'|'error' }
  const [files, setFiles] = useState([]);
  const [nodeStats, setNodeStats] = useState({});
  const [statuses, setStatuses] = useState({}); // { nodeId: status }
  const inputRef = useRef(null);
  const base = nodeBases[selectedNode];
  const nodeIds = Object.keys(nodeBases);

  // Poll health/status for selected node (details panel)
  useEffect(() => {
    let stop = false;
    async function tick() {
      try {
        const [health, status] = await Promise.all([getHealth(base), getStatus(base)]);
        setNodeStats({ health, status });
      } catch {}
      if (!stop) setTimeout(tick, 3000);
    }
    tick();
    return () => { stop = true; };
  }, [base]);

  // Aggregate statuses for all nodes (network stats)
  useEffect(() => {
    let stop = false;
    async function tickAll() {
      const entries = await Promise.all(
        nodeIds.map(async (id) => {
          try { return [id, await getStatus(nodeBases[id])]; }
          catch { return [id, null]; }
        })
      );
      const map = Object.fromEntries(entries);
      setStatuses(map);
      if (!stop) setTimeout(tickAll, 3000);
    }
    tickAll();
    return () => { stop = true; };
  }, []);

  // Connect SSE to all nodes once; store events per node
  useEffect(() => {
    const sources = [];
    nodeIds.forEach((id) => {
      try {
        // mark connecting for this node
        setSSEStatus((m) => ({ ...m, [id]: m[id] || 'connecting' }));

        const es = connectEvents(nodeBases[id], (evt) => {
          // Always group activities under the UI node id (node1/node2/node3)
          const nodeKey = id;

          // Friendlier labels for common events
          const eventName = evt?.event || 'event';
          const labelMap = {
            file_stored: 'uploaded',
            chunk_stored: 'chunk stored',
            manifest_cached: 'manifest cached',
            chunk_cached: 'chunk cached',
            file_served: evt?.remote ? 'fetched (remote)' : 'served',
            file_deleted: 'deleted',
            http_fetch_attempt: 'fetching',
            http_fetch_success: 'fetched',
            http_fetch_error: 'fetch error',
            rpc_ping_out: 'ping →',
            rpc_ping_in: 'ping ←',
            rpc_ping_ok: 'ping ok',
            rpc_ping_err: 'ping error',
            rpc_find_node_out: 'find_node →',
            rpc_find_node_in: 'find_node ←',
            rpc_find_node_ok: 'find_node ok',
            rpc_find_node_err: 'find_node error',
            rpc_find_providers_out: 'find_providers →',
            rpc_find_providers_in: 'find_providers ←',
            rpc_find_providers_ok: 'find_providers ok',
            rpc_find_providers_err: 'find_providers error',
            rpc_store_provider_out: 'store_provider →',
            rpc_store_provider_in: 'store_provider ←',
            rpc_store_provider_ok: 'store_provider ok',
            rpc_store_provider_err: 'store_provider error',
            chunk_deleted: 'chunk deleted',
            sse_open: 'connected',
            sse_error: 'connection error',
            hello: 'connected',
          };
          const label = labelMap[eventName] || eventName;

          const ref = evt?.manifest || evt?.hash || '';
          const refShort = ref ? String(ref).slice(0, 10) + '…' : '';
          const peer = evt?.to || evt?.from || evt?.http || '';
          const peerStr = peer ? String(peer) : '';
          const providers = (typeof evt?.providers_deleted !== 'undefined')
            ? ` ${evt.providers_deleted}/${evt.providers_contacted}`
            : '';

          // Include real backend node_id in message for clarity
          const backendNode = evt?.node_id ? ` (${evt.node_id})` : '';
          const text = `${nodeKey}${backendNode}: ${label}${refShort ? ` ${refShort}` : ''}${peerStr ? ` ${peerStr}` : ''}${providers}`;

          const item = { id: Date.now() + Math.random(), ts: new Date().toLocaleTimeString(), text };
          setActivitiesByNode((prev) => {
            const next = { ...prev };
            const arr = next[nodeKey] ? [item, ...next[nodeKey]] : [item];
            next[nodeKey] = arr.slice(0, 50);
            return next;
          });

          // Track SSE connection state
          if (eventName === 'sse_open' || eventName === 'hello') {
            setSSEStatus((m) => ({ ...m, [id]: 'open' }));
          }
          if (eventName === 'sse_error') {
            setSSEStatus((m) => ({ ...m, [id]: 'error' }));
          }
        });
        sources.push(es);
      } catch (err) {
        // Surface constructor failures in the UI feed and status
        const item = { id: Date.now() + Math.random(), ts: new Date().toLocaleTimeString(), text: `${id}: connection error (${err?.message || 'EventSource failed'})` };
        setActivitiesByNode((prev) => {
          const next = { ...prev };
          const arr = next[id] ? [item, ...next[id]] : [item];
          next[id] = arr.slice(0, 50);
          return next;
        });
        setSSEStatus((m) => ({ ...m, [id]: 'error' }));
      }
    });
    return () => { sources.forEach((es) => es && es.close && es.close()); };
  }, []);

  const onUploadClick = () => inputRef.current?.click();

  const onFileSelected = async (e) => {
    const file = e.target.files?.[0];
    if (!file) return;
    setFiles((f) => [
      { name: file.name, size: `${(file.size / 1048576).toFixed(2)} MB`, chunks: "…", hash: "…", uploading: true },
      ...f,
    ]);
    try {
      const resp = await uploadFile(base, file, true);
      setFiles((f) => [
        { name: file.name, size: `${(file.size / 1048576).toFixed(2)} MB`, chunks: resp.chunks, hash: resp.hash, uploading: false },
        ...f.slice(1),
      ]);
      setNetworkActivity((a) => [
        { id: Date.now(), text: `Uploaded ${file.name} → ${resp.hash.slice(0, 10)}…`, ts: new Date().toLocaleTimeString() },
        ...a.slice(0, 49),
      ]);
      // Also surface locally in the selected node feed (even if SSE fails)
      const item = { id: Date.now() + Math.random(), ts: new Date().toLocaleTimeString(), text: `${selectedNode}: uploaded ${resp.hash.slice(0,10)}…` };
      setActivitiesByNode((prev) => {
        const next = { ...prev };
        const arr = next[selectedNode] ? [item, ...next[selectedNode]] : [item];
        next[selectedNode] = arr.slice(0, 50);
        return next;
      });
    } catch (err) {
      setFiles((f) => f.slice(1));
      alert(err.message);
    } finally {
      e.target.value = "";
    }
  };

  const getNodeColor = () => "#10b981"; // all active in demo
  const nodesViz = [
    { id: "node1", x: 150, y: 200 },
    { id: "node2", x: 400, y: 150 },
    { id: "node3", x: 600, y: 250 },
  ];

  const selectedFeed = activitiesByNode[selectedNode] || [];
  const totalChunks = Object.values(statuses).reduce((sum, s) => sum + (s?.storage?.current_chunks || 0), 0);
  const totalBytes = Object.values(statuses).reduce((sum, s) => sum + (s?.storage?.current_bytes || 0), 0);

  return (
    <div className="min-h-screen text-white p-6">
      <div className="flex items-center justify-between mb-8">
        <div className="flex items-center space-x-3">
          <Network className="w-8 h-8 text-blue-400" />
          <div>
            <h1 className="text-3xl font-bold">Mini-IPFS Dashboard</h1>
            <p className="text-gray-400">Distributed Content Network Monitor</p>
          </div>
        </div>
        <div className="flex items-center space-x-4">
          <select className="bg-gray-800 px-3 py-2 rounded" value={selectedNode} onChange={(e) => setSelectedNode(e.target.value)}>
            {Object.keys(nodeBases).map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
          <div className="bg-green-500 w-3 h-3 rounded-full animate-pulse" />
          <span className="text-sm">Connected</span>
        </div>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-3 gap-6">
        <div className="xl:col-span-2 bg-gray-800 rounded-lg p-6">
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-xl font-semibold flex items-center">
              <Network className="w-5 h-5 mr-2 text-blue-400" /> Network Topology
            </h2>
          </div>
          <div className="relative h-96 bg-gray-900 rounded border overflow-hidden">
            <svg className="absolute inset-0 w-full h-full">
              <line x1="150" y1="200" x2="400" y2="150" stroke="#4b5563" strokeWidth="2" opacity="0.6" />
              <line x1="400" y1="150" x2="600" y2="250" stroke="#4b5563" strokeWidth="2" opacity="0.6" />
              <circle r="4" fill="#60a5fa">
                <animateMotion dur="3s" repeatCount="indefinite"><path d="M150,200 L400,150" /></animateMotion>
              </circle>
            </svg>
            {nodesViz.map((n) => (
              <div
                key={n.id}
                className={`absolute -translate-x-1/2 -translate-y-1/2 text-center cursor-pointer transition-transform ${selectedNode === n.id ? 'scale-110' : ''}`}
                style={{ left: n.x, top: n.y }}
                onClick={() => setSelectedNode(n.id)}
              >
                <div className="w-12 h-12 rounded-full border-4 flex items-center justify-center" style={{ borderColor: getNodeColor() }}>
                  <Server className="w-6 h-6" style={{ color: getNodeColor() }} />
                </div>
                <div className="mt-2 text-xs">{n.id}</div>
              </div>
            ))}
          </div>
        </div>

        <div className="space-y-6">
          <div className="bg-gray-800 rounded-lg p-6">
            <h3 className="text-lg font-semibold mb-4 flex items-center">
              <Eye className="w-5 h-5 mr-2 text-green-400" /> {selectedNode} Details
            </h3>
            <div className="space-y-3 text-sm">
              <div className="flex justify-between"><span className="text-gray-400">Chunks:</span><span className="font-mono">{nodeStats.status?.storage?.current_chunks ?? "…"}</span></div>
              <div className="flex justify-between"><span className="text-gray-400">Bytes:</span><span className="font-mono">{nodeStats.status?.storage?.current_bytes ?? "…"}</span></div>
              <div className="flex justify-between"><span className="text-gray-400">HTTP:</span><span className="font-mono">{nodeStats.status?.http_addr ?? "…"}</span></div>
              <div className="flex justify-between"><span className="text-gray-400">DHT:</span><span className="font-mono">{nodeStats.status?.dht_addr ?? "…"}</span></div>
            </div>
          </div>

          <div className="bg-gray-800 rounded-lg p-6">
            <h3 className="text-lg font-semibold mb-4 flex items-center">
              <Activity className="w-5 h-5 mr-2 text-blue-400" /> Live Activity
            </h3>
            <div className="space-y-2 max-h-64 overflow-y-auto">
              {selectedFeed.length === 0 ? (
                <div className="text-gray-500 text-sm">
                  {sseStatus[selectedNode] === 'error' && 'Connection error. Check node address/CORS.'}
                  {sseStatus[selectedNode] === 'open' && 'Connected. Waiting for activity…'}
                  {!sseStatus[selectedNode] && 'Connecting to event stream…'}
                </div>
              ) : (
                selectedFeed.map((a) => (
                  <div key={a.id} className="flex items-start space-x-2 text-sm">
                    <div className="text-gray-500 text-xs mt-0.5 font-mono">{a.ts}</div>
                    <div className="text-gray-300 flex-1">{a.text}</div>
                  </div>
                ))
              )}
            </div>
          </div>
        </div>

        <div className="xl:col-span-2 bg-gray-800 rounded-lg p-6">
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-xl font-semibold flex items-center"><FileText className="w-5 h-5 mr-2 text-purple-400" /> Distributed Files</h2>
            <button onClick={onUploadClick} className="px-4 py-2 bg-purple-600 rounded flex items-center hover:bg-purple-700">
              <Upload className="w-4 h-4 mr-2" /> Upload File
            </button>
            <input ref={inputRef} type="file" className="hidden" onChange={onFileSelected} />
          </div>
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-gray-400 border-b border-gray-700">
                <tr><th className="text-left py-3">File</th><th className="text-left py-3">Size</th><th className="text-left py-3">Chunks</th><th className="text-left py-3">Manifest</th><th className="text-left py-3">Action</th></tr>
              </thead>
              <tbody>
                {files.map((f, i) => (
                  <tr key={i} className="border-b border-gray-700 hover:bg-gray-750">
                    <td className="py-3">{f.name}</td>
                    <td className="py-3 text-gray-400">{f.size}</td>
                    <td className="py-3 font-mono">{f.chunks}</td>
                    <td className="py-3 font-mono text-blue-400">{f.hash?.slice(0, 12)}…</td>
                    <td className="py-3">
                      {f.uploading ? (
                        <span className="text-blue-400">Uploading…</span>
                      ) : f.hash ? (
                        <div className="flex items-center gap-4">
                          <a className="text-green-400 flex items-center" href={downloadURL(base, f.hash, true)} target="_blank" rel="noreferrer" onClick={() => {
                            const item = { id: Date.now() + Math.random(), ts: new Date().toLocaleTimeString(), text: `${selectedNode}: fetch requested ${f.hash.slice(0,10)}…` };
                            setActivitiesByNode((prev) => {
                              const next = { ...prev };
                              const arr = next[selectedNode] ? [item, ...next[selectedNode]] : [item];
                              next[selectedNode] = arr.slice(0, 50);
                              return next;
                            });
                          }}>
                            <Download className="w-4 h-4 mr-1" />Download
                          </a>
                          <button className="text-red-400" onClick={async () => {
                            try {
                              const res = await deleteFile(base, f.hash);
                              setFiles((arr) => arr.filter((x) => x.hash !== f.hash));
                              setNetworkActivity((a) => [
                                { id: Date.now(), text: `Deleted ${f.name} (${f.hash.slice(0,10)}…) providers: ${res.providers_deleted ?? 0}/${res.providers_contacted ?? 0}`, ts: new Date().toLocaleTimeString() },
                                ...a.slice(0,49),
                              ]);
                              const item = { id: Date.now() + Math.random(), ts: new Date().toLocaleTimeString(), text: `${selectedNode}: deleted ${f.hash.slice(0,10)}… ${res.providers_deleted ?? 0}/${res.providers_contacted ?? 0}` };
                              setActivitiesByNode((prev) => {
                                const next = { ...prev };
                                const arr = next[selectedNode] ? [item, ...next[selectedNode]] : [item];
                                next[selectedNode] = arr.slice(0, 50);
                                return next;
                              });
                            } catch (e) {
                              alert(e.message);
                            }
                          }}>Delete</button>
                        </div>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>

        <div className="bg-gray-800 rounded-lg p-6">
          <h3 className="text-lg font-semibold mb-4 flex items-center"><Zap className="w-5 h-5 mr-2 text-yellow-400" /> Network Stats</h3>
          <div className="space-y-2 text-sm text-gray-300">
            <div>Total Chunks: <span className="font-mono">{totalChunks}</span></div>
            <div>Total Bytes: <span className="font-mono">{totalBytes}</span></div>
            <div>Selected Node: <span className="font-mono">{nodeStats.health?.node_id ?? "…"}</span></div>
            <div>Version: {nodeStats.health?.version ?? "…"}</div>
          </div>
        </div>
      </div>
    </div>
  );
}
