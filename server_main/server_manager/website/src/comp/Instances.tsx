import React, { useEffect, useRef, useState, type JSX } from "react";
import { motion, AnimatePresence, useMotionValue, useSpring } from "framer-motion";
import { Play, RefreshCw, Trash2, Save, CloudDownload, Loader2 } from "lucide-react";

/* ---------------------- Types ---------------------- */
// Updated Instance type to match Go struct
type Instance = {
  name: string;
  players: string[] | null; // Can be null
  player_count: number;
  tps: number;
  port: number;
  status: string;
};

type InstanceManager = {
  state: string;
  domain: string;
  name: string;
  cpu_percent: number;
  ram_used_mb: number;
  ram_total_mb: number;
  instances: Instance[];
};

// NEW: Type for the local system info
type LocalSystemInfo = {
  cpu_percent: number;
  ram_used_mb: number;
  ram_total_mb: number;
};

// NEW: Type for the whole /status response
type GlobalSummary = {
  proxy: Record<string, any>;
  system: LocalSystemInfo;
  managers: InstanceManager[];
};

type UseAnimatedNumberOptions = {
  fallback?: number;          // value to show when input is not finite (default 0)
  animateFallback?: boolean;  // whether to animate to fallback or set immediately (default true)
};


/* ---------------------- small animated number hook ---------------------- */
function useAnimatedNumber(
  value: number,
  precision = 0,
  opts: UseAnimatedNumberOptions = {}
) {
  const { fallback = 0, animateFallback = true } = opts;

  // initialize motion value with a finite number (value or fallback)
  const initial = Number.isFinite(value) ? value : fallback;
  const mv = useMotionValue<number>(initial);
  const spring = useSpring(mv, { stiffness: 140, damping: 20 });

  const [display, setDisplay] = useState<number>(initial);
  const warnedRef = useRef(false);

  // When `value` changes, decide what to do
  useEffect(() => {
    if (Number.isFinite(value)) {
      // normal case: animate to the new finite value
      mv.set(value);
    } else {
      // invalid input: optionally warn once and move to fallback
      if (!warnedRef.current) {
        console.warn("useAnimatedNumber: received non-finite value", value);
        warnedRef.current = true;
      }

      if (animateFallback) {
        // animate toward the fallback so UI shows smooth transition
        mv.set(fallback);
      } else {
        // immediate fallback: set motion value and display immediately
        mv.set(fallback);
        setDisplay(fallback);
      }
    }
    // we intentionally do not clear warnedRef; if you want repeated warnings remove the ref guard
  }, [value, mv, fallback, animateFallback]);

  // Subscribe to spring changes and update display with guards
  useEffect(() => {
    const unsub = spring.on("change", (v) => {
      if (Number.isFinite(v)) {
        setDisplay(Number(v.toFixed(precision)));
      } else {
        // if spring emits non-finite (rare), set the safe fallback so we don't "stick"
        setDisplay(fallback);
      }
    });
    return () => unsub();
  }, [spring, precision, fallback]);

  return display;
}

/* ---------------------- UI helpers (header / bg) ---------------------- */
const NeonBackground: React.FC = () => (
  <>
    <div className="fixed inset-0 -z-10">
      <div className="absolute inset-0 bg-gradient-to-b from-black via-[#07030a] to-[#0b0410]" />
      <div className="absolute -top-48 -left-48 w-[640px] h-[640px] rounded-full bg-gradient-to-br from-purple-700/18 to-purple-900/6 blur-3xl pointer-events-none" />
      {/* subtle tile grid */}
      <div className="absolute inset-0 grid grid-cols-12 gap-[1px] opacity-5 pointer-events-none">
        {Array.from({ length: 12 * 12 }).map((_, i) => (
          <div key={i} className="bg-purple-600/8" />
        ))}
      </div>
    </div>
  </>
);

/* ---------------------- NEW: Local System Stats ---------------------- */
const LocalSystemStats: React.FC<{
  systemInfo: LocalSystemInfo | null;
  proxyData: Record<string, any> | null;
}> = ({ systemInfo, proxyData }) => {
  const cpu = Math.min(100, Math.round(systemInfo?.cpu_percent ?? 0));
  const ram = Math.min(
    100,
    Math.round(((systemInfo?.ram_used_mb ?? 0) / Math.max(1, systemInfo?.ram_total_mb ?? 1)) * 100)
  );
  const players = proxyData?.players_total ?? 0;

  const animatedCPU = useAnimatedNumber(cpu);
  const animatedRAM = useAnimatedNumber(ram);
  const animatedPlayers = useAnimatedNumber(players);

  return (
    <motion.div
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      transition={{ duration: 0.5 }}
      className="mb-8 p-6 bg-black/30 rounded-2xl border border-purple-800/20 grid grid-cols-1 md:grid-cols-3 gap-6"
    >
      <StatBox title="Proxy Players" value={animatedPlayers} unit="" gradient="from-cyan-400 to-blue-500" />
      <StatBox title="Local CPU" value={animatedCPU} unit="%" gradient="from-green-400 to-green-600" />
      <StatBox title="Local RAM" value={animatedRAM} unit="%" gradient="from-blue-400 to-purple-500" />
    </motion.div>
  );
};

const StatBox: React.FC<{ title: string; value: number; unit: string; gradient: string }> = ({
  title,
  value,
  unit,
  gradient,
}) => (
  <div className="flex items-center gap-4">
    <div
      className={`w-12 h-12 rounded-lg bg-gradient-to-br ${gradient} flex items-center justify-center text-white shadow-lg`}
    >
      <span className="text-xl font-bold">{value}</span>
      <span className="text-xs font-bold -mt-2 ml-0.5">{unit}</span>
    </div>
    <div>
      <div className="text-white font-semibold">{title}</div>
      <div className="text-xs text-purple-200/60">
        {title === "Proxy Players"
          ? "Total players on proxy"
          : title === "Local CPU"
          ? "Go backend CPU usage"
          : "Go backend RAM usage"}
      </div>
    </div>
  </div>
);

/* ---------------------- Main Component ---------------------- */
export default function InstancesPage(): JSX.Element {
  // State now includes all parts of the new /status response
  const [instanceManagers, setInstanceManagers] = useState<InstanceManager[]>([]);
  const [localSystemInfo, setLocalSystemInfo] = useState<LocalSystemInfo | null>(null);
  const [proxyData, setProxyData] = useState<Record<string, any> | null>(null);

  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<string | null>(null);

  const [showModal, setShowModal] = useState(false);
  const [newDomain, setNewDomain] = useState("");
  const [newName, setNewName] = useState("");
  const [formError, setFormError] = useState<string | null>(null);

  // toast
  const [toast, setToast] = useState<{ type: "ok" | "error"; text: string } | null>(null);
  useEffect(() => {
    if (!toast) return;
    const id = setTimeout(() => setToast(null), 3500);
    return () => clearTimeout(id);
  }, [toast]);

  // initial fetch + polling
  const fetchInstanceManagers = async () => {
    setError(null);
    try {
      // UPDATED: Fetch from /api/status
      const res = await fetch("/api/status");
      if (!res.ok) throw new Error(`HTTP ${res.status}`);

      // UPDATED: Parse the new GlobalSummary response
      const data: GlobalSummary = await res.json();

      // UPDATED: Set all pieces of state
      setInstanceManagers(data.managers || []);
      setLocalSystemInfo(data.system || null);
      setProxyData(data.proxy || null);
    } catch (err: any) {
      setError(err.message || "Failed to fetch");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    setLoading(true);
    fetchInstanceManagers();
    const id = setInterval(fetchInstanceManagers, 2_000);
    return () => clearInterval(id);
  }, []);

  // add IM
  // NOTE: The Go backend file does not have an endpoint for /api/create_im
  // This function will fail until that endpoint is created.
  const handleAddInstanceManager = async () => {
    setFormError(null);
    if (!newDomain.trim() || !newName.trim()) {
      setFormError("Domain + Name required");
      return;
    }

    try {
      const res = await fetch("/api/create_im", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ domain: newDomain.trim(), name: newName.trim() }),
      });
      if (!res.ok) throw new Error(`Server ${res.status}`);
      setToast({ type: "ok", text: "Instance Manager created" });
      setShowModal(false);
      setNewDomain("");
      setNewName("");
      fetchInstanceManagers(); // Refresh data
    } catch (err: any) {
      setFormError(err.message || "Create failed");
    }
  };

  // delete IM
  // NOTE: The Go backend file does not have an endpoint for /api/delete_im
  // This function will fail until that endpoint is created.
  const handleDeleteInstanceManager = async (im: InstanceManager) => {
    // Replaced confirm() with a simple prompt for safety
    const confirmed = window.prompt(`Type DELETE to confirm deleting "${im.name}"`);
    if (confirmed !== "DELETE") {
      setToast({ type: "error", text: "Deletion cancelled" });
      return;
    }

    const prev = instanceManagers;
    setInstanceManagers((s) => s.filter((x) => !(x.domain === im.domain && x.name === im.name)));

    try {
      const res = await fetch("/api/delete_im", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ domain: im.domain, name: im.name }),
      });
      if (!res.ok) throw new Error(`Delete failed ${res.status}`);
      setToast({ type: "ok", text: "Deleted" });
    } catch (err: any) {
      setInstanceManagers(prev);
      setToast({ type: "error", text: `Delete failed: ${err.message || err}` });
    }
  };

  // generic action (start/stop/restart) at manager-level
  // NOTE: The Go backend file does not have an endpoint for /api/action
  // This function will fail until that endpoint is created.
  const handleInstanceAction = async (im: InstanceManager, instance: Instance | null, action: "restart" | "save" | "pluginUpdate") => {

    if (action == "pluginUpdate") {
      try {
        // optimistic UI could be added; here we call API and show toast
        console.log("Logging: " + JSON.stringify({ domain: im.domain, action }));

        const res = await fetch("/api/action", {
          method: "POST",
          headers: { "Content-Type": "application/json" }, // Corrected "jsoSn"
          body: JSON.stringify({ domain: im.domain, action, name: "none" }),
        });
        if (!res.ok) throw new Error(`Action failed ${res.status}`);
        setToast({ type: "ok", text: `${action} requested` });
        // optional refresh
        fetchInstanceManagers();
      } catch (err: any) {
        setToast({ type: "error", text: `Action error: ${err.message || err}` });
      }
    } else {
      if (instance != null) {
        try {
          // optimistic UI could be added; here we call API and show toast
          console.log("Logging: " + JSON.stringify({ domain: im.domain, name: instance.name, action }));

          const res = await fetch("/api/action", {
            method: "POST",
            headers: { "Content-Type": "application/json" }, // Corrected "jsoSn"
            body: JSON.stringify({ domain: im.domain, name: instance.name, action }),
          });
          if (!res.ok) throw new Error(`Action failed ${res.status}`);
          setToast({ type: "ok", text: `${action} requested` });
          // optional refresh
          fetchInstanceManagers();
        } catch (err: any) {
          setToast({ type: "error", text: `Action error: ${err.message || err}` });
        }
      }
    }
  };

  return (
    <div className="min-h-screen text-white">
      <NeonBackground />
      <header className="w-full fixed top-0 left-0 z-30 backdrop-blur-md bg-black/30 border-b border-white/6">
        <div className="max-w-6xl mx-auto px-6 py-3 flex items-center justify-between gap-4">
          <div className="flex items-center gap-4">
            <div className="w-18 h-14 rounded-lg bg-gradient-to-br from-purple-700 to-purple-500 flex items-center justify-center shadow-xl">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-6 w-6 text-white" viewBox="0 0 24 24" fill="currentColor">
                <path d="M12 2L3 7v6c0 5 3.58 9.74 9 11 5.42-1.26 9-6 9-11V7l-9-5z" />
              </svg>
            </div>
            <div>
              <h2 className="text-3xl font-bold text-white">Instance Managers</h2>
              <p className="text-sm text-purple-200/70">Overview · Manage, start & monitor your IMs</p>
            </div>
          </div>

          <nav className="flex items-center gap-3">
            <button
              onClick={() => {
                setFormError(null);
                setNewDomain("");
                setNewName("");
                setShowModal(true);
              }}
              className="inline-flex items-center gap-2 px-3 py-2 rounded-lg bg-gradient-to-br from-green-600 to-green-500 cursor-pointer text-white shadow-lg hover:scale-99 active:scale-95 transition"
            >
              <Play size={16} /> Add IM
            </button>

            <button
              onClick={() => fetchInstanceManagers()}
              className="px-3 py-2 rounded-lg bg-white/5 border border-white/6 text-sm hover:bg-white/6 transition cursor-pointer"
            >
              Refresh
            </button>
          </nav>
        </div>
      </header>
      <main className="pt-24 px-6 pb-16 max-w-6xl mx-auto">
        <div className="mb-6 flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
          </div>
        </div>

        {/* NEW: Local Stats Display */}
        <AnimatePresence>
          {!loading && !error && <LocalSystemStats systemInfo={localSystemInfo} proxyData={proxyData} />}
        </AnimatePresence>

        {/* list */}
        {loading && <div className="animate-pulse text-purple-200/60">Loading…</div>}
        {error && <div className="text-red-400">{error}</div>}

        {!loading && !error && (
          <div className="flex flex-col gap-4">
            {instanceManagers.length === 0 ? (
              <div className="text-center text-purple-200/40 italic py-8 rounded-xl bg-black/30 border border-purple-800/20">
                No Instance Managers yet — add one with the button above
              </div>
            ) : (
              instanceManagers.map((im, i) => (
                <ManagerCard
                  key={im.domain + "::" + im.name}
                  im={im}
                  index={i}
                  onDelete={() => handleDeleteInstanceManager(im)}
                  onInstanceAction={(instance, action) => handleInstanceAction(im, instance, action)}
                />
              ))
            )}
          </div>
        )}

        {/* modal */}
        <AnimatePresence>
          {showModal && (
            <motion.div
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              className="fixed inset-0 z-50 flex items-start justify-center pt-24"
            >
              <div className="absolute inset-0 bg-black/60" onClick={() => setShowModal(false)} />

              <motion.div
                initial={{ y: -8, scale: 0.98 }}
                animate={{ y: 0, scale: 1 }}
                exit={{ y: -8, scale: 0.98, opacity: 0 }}
                transition={{ duration: 0.18 }}
                className="relative z-10 w-full max-w-md p-6 bg-gradient-to-br from-black/75 to-purple-950/70 rounded-2xl shadow-2xl border border-purple-800/40"
                onKeyDown={(e) => e.key === "Escape" && setShowModal(false)}
              >
                <h3 className="text-lg font-semibold text-white mb-3">Add Instance Manager</h3>

                <form
                  onSubmit={(e) => {
                    e.preventDefault();
                    handleAddInstanceManager();
                  }}
                  className="flex flex-col gap-3"
                >
                  <input
                    placeholder="Domain (example.local:25566)"
                    value={newDomain}
                    onChange={(e) => setNewDomain(e.target.value)}
                    className="p-2 rounded bg-[#120b1a] text-white placeholder-purple-300 border border-purple-800/30"
                    autoFocus
                  />
                  <input
                    placeholder="Name (My Manager)"
                    value={newName}
                    onChange={(e) => setNewName(e.target.value)}
                    className="p-2 rounded bg-[#120b1a] text-white placeholder-purple-300 border border-purple-800/30"
                  />

                  {formError && <div className="text-red-400 text-sm">{formError}</div>}

                  <div className="flex justify-end gap-2 pt-2">
                    <button type="button" onClick={() => setShowModal(false)} className="px-3 py-1.5 rounded bg-gray-700">
                      Cancel
                    </button>
                    <button
                      type="submit"
                      className="px-3 py-1.5 rounded bg-green-600 text-white disabled:opacity-50"
                      disabled={!newDomain.trim() || !newName.trim()}
                    >
                      Add
                    </button>
                  </div>
                </form>
              </motion.div>
            </motion.div>
          )}
        </AnimatePresence>

        {/* toast */}
        <AnimatePresence>
          {toast && (
            <motion.div
              initial={{ y: 12, opacity: 0 }}
              animate={{ y: 0, opacity: 1 }}
              exit={{ y: 12, opacity: 0 }}
              className={`fixed right-6 bottom-6 z-50 rounded-lg px-4 py-2 shadow-xl ${
                toast.type === "ok" ? "bg-green-600/90 text-white" : "bg-red-600/90 text-white"
              }`}
            >
              {toast.text}
            </motion.div>
          )}
        </AnimatePresence>
      </main>
    </div>
  );
}

/* ---------------------- Manager Card (presentational + actions) ---------------------- */
function ManagerCard({
  im,
  index,
  onDelete,
  onInstanceAction,
}: {
  im: InstanceManager;
  index: number;
  onDelete: () => void;
  onInstanceAction(instance: Instance | null, action: "restart" | "save" | "pluginUpdate"): void;
}) {
  const cpu = Math.min(100, Math.round(im.cpu_percent));
  const ram = Math.min(100, Math.round((im.ram_used_mb / Math.max(1, im.ram_total_mb)) * 100));
  const animatedCPU = useAnimatedNumber(cpu);
  const animatedRAM = useAnimatedNumber(ram);

  const [isUpdating, setIsUpdating] = useState(false);

  return (
    <motion.div
      initial={{ opacity: 0, y: 10 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.35, delay: index * 0.06 }}
      className="relative p-8 bg-gradient-to-br from-black/50 to-purple-950/50 rounded-2xl shadow-xl border border-purple-950/40 flex flex-col md:flex-row gap-6"
    >

      <div className="md:w-64 flex-shrink-0 text-white">
        <div className="flex items-start justify-between gap-2">
          <div>
            <h4 className="text-lg font-semibold truncate">{im.name}</h4>
            <p className="text-xs text-purple-200/70 truncate">{im.domain}</p>
          </div>

          <span
            className={`ml-2 px-2 py-0.5 text-xs font-semibold rounded-full ${
              im.state === "Online"
                ? "bg-green-600 text-white"
                : im.state === "Warning"
                ? "bg-yellow-400 text-black"
                : "bg-red-600 text-white"
            }`}
          >
            {im.state}
          </span>
        </div>

        <div className="mt-4">
          <div className="flex justify-between text-xs text-purple-200/70 mb-1">
            <span>CPU</span>
            <span>{animatedCPU}%</span>
          </div>
          <div className="w-full h-2 rounded overflow-hidden bg-black/40 border border-purple-800/20">
            <motion.div
              className="h-2 rounded"
              animate={{ width: `${cpu}%` }}
              transition={{ type: "spring", stiffness: 120, damping: 22 }}
              style={{ background: "linear-gradient(90deg,#16a34a,#22c55e)" }}
            />
          </div>
        </div>

        <div className="mt-4">
          <div className="flex justify-between text-xs text-purple-200/70 mb-1">
            <span>RAM</span>
            <span>{animatedRAM}%</span>
          </div>
          <div className="w-full h-2 rounded overflow-hidden bg-black/40 border border-purple-800/20">
            <motion.div
              className="h-2 rounded"
              animate={{ width: `${ram}%` }}
              transition={{ type: "spring", stiffness: 120, damping: 22 }}
              style={{ background: "linear-gradient(90deg,#3b82f6,#60a5fa)" }}
            />
          </div>
        </div>

        <div className="pt-4.5 z-10 flex items-center gap-2">
        <button
          onClick={() => {
            setIsUpdating(true)
            onInstanceAction(null, "pluginUpdate")}
          }
          aria-label={`Update plugins for ${im.name}`}
          title={`Update plugins for ${im.name}`}
          disabled={isUpdating}
          className="flex items-center gap-2 px-3 py-1.5 rounded-md bg-indigo-700/90 text-white text-xs hover:brightness-105 transition cursor-pointer"
        >
          {isUpdating ? (
            <>
              <Loader2 className="w-4 h-4 animate-spin" />
              <span>Updating...</span>
            </>
          ) : (
            <>
              <CloudDownload size={14} />
              <span>Update Plugins</span>
            </>
          )}
        </button>

        <button
          onClick={onDelete}
          aria-label={`Delete ${im.name}`}
          title={`Delete ${im.name}`}
          className="flex items-center gap-2 px-3 py-1.5 rounded-md bg-red-700/90 text-white text-xs hover:bg-red-600 cursor-pointer transition"
        >
          <Trash2 size={16} />
          <span>Delete</span>
        </button>
      </div>
      </div>

      <div className="flex-1 space-y-3">
        {(!im.instances || im.instances.length === 0) && (
          <div className="text-xs text-purple-200/60 italic p-4 bg-black/20 rounded">
            No instances yet
          </div>
        )}

        {im.instances?.map((inst) => (
          <div
            key={inst.name}
            className="bg-[#12061b] rounded-xl shadow-inner text-white flex flex-col md:flex-row md:items-center justify-between p-4 min-h-[64px] border border-purple-800/30 gap-3"
          >
            <div className="flex items-center gap-3">
              <div
                className={`w-3 h-3 rounded-full ${
                  inst.status == "running" ? "bg-green-400" : "bg-red-500"
                } ${inst.status == "running" ? "animate-pulse" : ""}`}
              />
              <div>
                <p className="text-base font-semibold truncate">{inst.name}</p>
                <p className="text-xs text-purple-200/60">TPS: {inst.tps}</p>
              </div>
            </div>

            <div className="text-xs text-purple-200/60">{inst.player_count} players</div>

            <div className="flex gap-2 mt-2 md:mt-0">
              <button
                onClick={() => onInstanceAction(inst, "restart")}
                className="px-3 py-1.5 rounded-md bg-blue-600/90 text-white text-xs flex items-center gap-2 hover:brightness-105 transition cursor-pointer"
              >
                <RefreshCw size={14} /> Restart
              </button>

              <button
                onClick={() => onInstanceAction(inst, "save")}
                className="px-3 py-1.5 rounded-md bg-purple-600/90 text-white text-xs flex items-center gap-2 hover:brightness-105 transition cursor-pointer"
              >
                <Save size={14} /> Save World
              </button>
            </div>
          </div>
        ))}
      </div>
    </motion.div>
  );
}