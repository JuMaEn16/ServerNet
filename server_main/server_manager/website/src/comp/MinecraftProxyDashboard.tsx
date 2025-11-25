import React, { useEffect, useMemo, useState } from "react";
import { motion, AnimatePresence, useMotionValue, useSpring } from "framer-motion";

// Enhanced React + TypeScript dashboard for a Minecraft Proxy with animations (Framer Motion + Tailwind)
// - Purple & black theme
// - No external chart libs required
// - Smooth animated stat numbers, bars, server rows and chart paths
// To use: `npm install framer-motion` in your project

// ---------------------- Types ----------------------
interface ServerState {
  id: string;
  name: string;
  online: boolean;
  players: number;
  maxPlayers: number;
  pingMs: number;
}

interface ProxyState {
  cpuPercent: number; // 0-100
  ramUsedMb: number;
  ramTotalMb: number;
  playerCount: number;
  maxPlayers: number;
  tps: number; // 0-20+
  servers: ServerState[];
  topPlayer?: string;
  updatedAt: string;
}

// ---------------------- API data -> our shape mapper ----------------------
function mapApiToProxyState(api: any): ProxyState {
  // The API shape (example):
  // {"proxy":{"players_total":0,"proxy_latency":0,"servers":[{"name":"lobby","players":0,"tps":-1}]},"system":{"cpu_percent":11.14,"ram_used_mb":15978,"ram_total_mb":63400}}
  const sys = api?.system ?? {};
  const proxy = api?.proxy ?? {};

  const cpuPercent = Math.round(Number(sys.cpu_percent ?? 0));
  const ramUsedMb = Math.round(Number(sys.ram_used_mb ?? 0));
  const ramTotalMb = Math.round(Number(sys.ram_total_mb ?? 1));
  const playerCount = Number(proxy.players_total ?? proxy.player_count ?? 0);

  const serversRaw: any[] = Array.isArray(proxy.servers) ? proxy.servers : [];

  // Map servers: the API doesn't include per-server ping/maxPlayers in the sample, so we use sensible defaults.
  const servers: ServerState[] = serversRaw.map((s: any) => {
    const name: string = String(s.name ?? "unknown");
    const players = Number(s.players ?? 0);
    // Some APIs use tps < 0 for offline; consider online only if tps > 0
    const tpsVal = Number(s.tps ?? -1);
    const online = tpsVal > 0;

    return {
      id: name.toLowerCase().replace(/\s+/g, "-"),
      name,
      online,
      players: players,
      // no maxPlayers provided in example — choose a reasonable default (you can adapt this)
      maxPlayers: Number(s.maxPlayers ?? 100),
      // proxy-wide latency as a fallback
      pingMs: Number(proxy.proxy_latency ?? 0),
    } as ServerState;
  });

  // Estimate TPS: average of reported positive per-server tps values, capped to 20
  const tpsValues = serversRaw.map((s: any) => Number(s.tps ?? -1)).filter((v) => v > 0);
  const tps = tpsValues.length === 0 ? 0 : Math.min(20, tpsValues.reduce((a, b) => a + b, 0) / tpsValues.length);

  // maxPlayers: no global field in example — we fall back to a default or sum of server maxPlayers
  const maxPlayers = Number((proxy.max_players ?? servers.reduce((a, b) => a + (b.maxPlayers ?? 0), 0)) || 500);

  return {
    cpuPercent,
    ramUsedMb,
    ramTotalMb,
    playerCount,
    maxPlayers,
    tps: Number(tps.toFixed(2)),
    servers,
    topPlayer: undefined,
    updatedAt: new Date().toISOString(),
  };
}

// ---------------------- API polling hook ----------------------
function useProxyApi(pollInterval = 2000) {
  const [state, setState] = useState<ProxyState>(() => ({
    cpuPercent: 0,
    ramUsedMb: 0,
    ramTotalMb: 1,
    playerCount: 0,
    maxPlayers: 500,
    tps: 0,
    servers: [],
    topPlayer: undefined,
    updatedAt: new Date().toISOString(),
  }));

  useEffect(() => {
    let mounted = true;
    const controller = new AbortController();

    async function fetchOnce() {
      try {
        const res = await fetch("/api/status", { signal: controller.signal });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const json = await res.json();
        const mapped = mapApiToProxyState(json);
        if (mounted) setState(mapped);
      } catch (e) {
        // keep previous state on error (could add retry/backoff or set an `error` field)
        // console.debug("/api/status error", e);
      }
    }

    // initial fetch + polling
    fetchOnce();
    const id = setInterval(fetchOnce, pollInterval);

    return () => {
      mounted = false;
      controller.abort();
      clearInterval(id);
    };
  }, [pollInterval]);

  return state;
}

// ---------------------- Animated number hook ----------------------
function useAnimatedNumber(value: number, precision = 0) {
  const mv = useMotionValue(value);
  const spring = useSpring(mv, { stiffness: 120, damping: 20 });
  const [display, setDisplay] = useState<number>(value);

  useEffect(() => {
    mv.set(value);
  }, [value, mv]);

  useEffect(() => {
    const unsubscribe = spring.on("change", (v: number) => {
      setDisplay(Number(v.toFixed(precision)));
    });
    return () => unsubscribe();
  }, [spring, precision]);

  return display;
}

// ---------------------- Small helper components (animated) ----------------------
function StatCard({ title, value, hint }: { title: string; value: React.ReactNode; hint?: string }) {
  return (
    <motion.div
      layout
      initial={{ opacity: 0, y: 8 }}
      animate={{ opacity: 1, y: 0 }}
      exit={{ opacity: 0, y: 6 }}
      transition={{ duration: 0.45, ease: "easeOut" }}
      className="bg-gradient-to-br from-black/60 to-purple-900/40 border border-purple-700/40 rounded-2xl p-4 shadow-xl backdrop-blur-sm"
    >
      <div className="text-sm text-purple-300/80">{title}</div>
      <div className="mt-1 text-2xl font-semibold text-white">{value}</div>
      {hint && <div className="mt-2 text-xs text-purple-200/60">{hint}</div>}
    </motion.div>
  );
}

function ServerRow({ s }: { s: ServerState }) {
  const onlinePulse = { scale: [1, 1.3, 1], opacity: [1, 0.7, 1] };
  return (
    <motion.div
      layout
      initial={{ opacity: 0, y: 6 }}
      animate={{ opacity: 1, y: 0 }}
      exit={{ opacity: 0, y: 6 }}
      transition={{ duration: 0.35 }}
      className="flex items-center justify-between gap-4 p-3 rounded-lg bg-black/40 border border-purple-800/30"
    >
      <div className="flex items-center gap-3">
        <motion.div animate={onlinePulse} transition={{ duration: 1.6, repeat: Infinity }} className={`w-3 h-3 rounded-full bg-green-400`} />
        <div className="font-medium text-white">{s.name}</div>
        <div className="text-xs text-purple-300/70">({s.id})</div>
      </div>
      <div className="flex items-center gap-6 text-sm text-purple-100">
        <div className="flex flex-col items-end">
          <motion.span className="text-white font-semibold" layout>
            {s.players}
          </motion.span>
          <span className="text-xs text-purple-300/60">/ {s.maxPlayers}</span>
        </div>
        <div className="text-xs text-purple-200/60">{s.pingMs} ms</div>
      </div>
    </motion.div>
  );
}

// ---------------------- Main Component ----------------------
export default function MinecraftProxyDashboard() {
  // switch from mock provider to real API polling
  const state = useProxyApi(2000);

  // resource percentages
  const ramPercent = useMemo(() => Math.round((state.ramUsedMb / Math.max(1, state.ramTotalMb)) * 100), [state.ramUsedMb, state.ramTotalMb]);

  // animated display numbers
  const animatedCPU = useAnimatedNumber(state.cpuPercent);
  const animatedRAM = useAnimatedNumber(Math.round(state.ramUsedMb / 1024));
  const animatedPlayers = useAnimatedNumber(state.playerCount);

  // small history for charts (in-memory)
  const [history, setHistory] = useState<{ t: string; cpu: number; ram: number; players: number }[]>([]);
  useEffect(() => {
    setHistory((h) => {
      const newPoint = { t: new Date(state.updatedAt).toLocaleTimeString(), cpu: state.cpuPercent, ram: ramPercent, players: state.playerCount };
      const next = [...h, newPoint].slice(-20);
      return next;
    });
  }, [state, ramPercent]);

  return (
    <div className="min-h-screen bg-gradient-to-b from-black via-black to-purple-950 text-white p-6">
      <div className="max-w-7xl mx-auto">
        <header className="flex items-center justify-between mb-6">
          <div className="flex items-center gap-4">
            <div className="w-14 h-14 rounded-lg bg-gradient-to-br from-purple-700 to-purple-500 flex items-center justify-center shadow-2xl">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-7 w-7 text-white" viewBox="0 0 24 24" fill="currentColor">
                <path d="M12 2L3 7v6c0 5 3.58 9.74 9 11 5.42-1.26 9-6 9-11V7l-9-5z" />
              </svg>
            </div>
            <div>
              <h1 className="text-2xl font-bold">Proxy Dashboard</h1>
              <p className="text-sm text-purple-300/70">Live overview</p>
            </div>
          </div>

          <div className="text-right">
            <div className="text-sm text-purple-300/70">Last update</div>
            <div className="font-medium">{new Date(state.updatedAt).toLocaleString()}</div>
          </div>
        </header>

        <main className="grid grid-cols-1 lg:grid-cols-3 gap-6">
          {/* Left column: Overview + stats */}
          <section className="lg:col-span-1 space-y-4">
            <div className="p-4 rounded-2xl bg-black/40 border border-purple-800/30 shadow-lg">
              <div className="grid grid-cols-2 gap-4">
                <StatCard title="CPU" value={<div className="flex items-baseline gap-2"><span>{animatedCPU}</span><span className="text-sm text-purple-300/70">%</span></div>} hint={`Approx. load`} />
                <StatCard title="RAM" value={<div className="flex items-baseline gap-2"><span>{animatedRAM}</span><span className="text-sm text-purple-300/70">GB</span></div>} hint={`${ramPercent}% of ${Math.round(state.ramTotalMb / 1024)} GB`} />
                <StatCard title="Players" value={<div className="flex items-baseline gap-2"><span>{animatedPlayers}</span></div>} hint={`Max ${state.maxPlayers}`} />
                <StatCard title="TPS" value={`${state.tps}`} hint={`Ticks per second`} />
              </div>

              <div className="mt-4">
                <div className="text-sm text-purple-300/70 mb-2">Resource bars</div>
                <div className="space-y-2">
                  <div>
                    <div className="text-xs text-purple-200/60 mb-1">CPU</div>
                    <div className="w-full h-3 bg-black/60 rounded-full overflow-hidden border border-purple-800/20">
                      <motion.div
                        className="h-3 rounded-full"
                        animate={{ width: `${state.cpuPercent}%` }}
                        transition={{ type: "spring", stiffness: 120, damping: 22 }}
                        style={{ background: "linear-gradient(90deg,#8b5cf6,#7c3aed)" }}
                      />
                    </div>
                  </div>
                  <div>
                    <div className="text-xs text-purple-200/60 mb-1">RAM</div>
                    <div className="w-full h-3 bg-black/60 rounded-full overflow-hidden border border-purple-800/20">
                      <motion.div
                        className="h-3 rounded-full"
                        animate={{ width: `${ramPercent}%` }}
                        transition={{ type: "spring", stiffness: 120, damping: 22 }}
                        style={{ background: "linear-gradient(90deg,#c084fc,#6d28d9)" }}
                      />
                    </div>
                  </div>
                </div>
              </div>
            </div>

            <div className="p-4 rounded-2xl bg-black/40 border border-purple-800/30 shadow-lg">
              <h3 className="text-sm text-purple-200/80 mb-3">Top info</h3>
              <div className="text-white font-medium">Top player: {state.topPlayer ?? "—"}</div>
              <div className="text-xs text-purple-300/70 mt-2">Proxy capacity: {state.maxPlayers} concurrent players</div>
            </div>
          </section>

          {/* Middle: Charts */}
          <section className="lg:col-span-2 space-y-4">
            <div className="p-4 rounded-2xl bg-black/40 border border-purple-800/30 shadow-lg">
              <h3 className="text-lg font-semibold mb-3">Live timeline</h3>

              {/* Animated SVG chart (no extra deps required) */}
              <div className="w-full h-40 bg-black/30 rounded-lg p-3 overflow-hidden">
                <MiniLineChart data={history} />
              </div>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              <div className="p-4 rounded-2xl bg-black/40 border border-purple-800/30 shadow-lg">
                <h4 className="text-sm text-purple-200/80 mb-3">Servers</h4>
                <div className="space-y-3">
                  <AnimatePresence>
                    {state.servers.map((s) => (
                      <ServerRow key={s.id} s={s} />
                    ))}
                  </AnimatePresence>
                </div>
              </div>

              <div className="p-4 rounded-2xl bg-black/40 border border-purple-800/30 shadow-lg">
                <h4 className="text-sm text-purple-200/80 mb-3">Quick actions</h4>
                <div className="flex flex-col gap-3">
                  <motion.button whileTap={{ scale: 0.97 }} className="px-4 py-2 rounded-lg bg-gradient-to-br from-purple-600 to-purple-500 hover:from-purple-500 hover:to-purple-400 text-white font-medium shadow cursor-pointer">Restart proxy</motion.button>
                  <motion.button whileHover={{ scale: 1.02 }} className="px-4 py-2 rounded-lg bg-black/60 border border-purple-700/40 text-purple-200 hover:text-white cursor-pointer">Refresh</motion.button>
                  <motion.button whileHover={{ scale: 1.02 }} className="px-4 py-2 rounded-lg bg-gradient-to-br from-red-600 to-red-500 text-white font-medium shadow cursor-pointer">Kick all guests</motion.button>
                </div>
              </div>
            </div>
          </section>
        </main>

        <footer className="mt-8 text-center text-xs text-purple-300/60">Made with ❤️ for Minecraft proxy monitoring • Theme: purple & black</footer>
      </div>
    </div>
  );
}

// ---------------------- MiniLineChart (animated paths) ----------------------
function MiniLineChart({ data }: { data: { t: string; cpu: number; ram: number; players: number }[] }) {
  const width = 900;
  const height = 160;
  const padding = 12;
  const pointsCpu = data.map((d, i) => ({ x: (i / Math.max(1, data.length - 1)) * (width - padding * 2) + padding, y: height - padding - (d.cpu / 100) * (height - padding * 2) }));
  const pointsRam = data.map((d, i) => ({ x: (i / Math.max(1, data.length - 1)) * (width - padding * 2) + padding, y: height - padding - (d.ram / 100) * (height - padding * 2) }));
  const pointsPlayers = data.map((d, i) => ({ x: (i / Math.max(1, data.length - 1)) * (width - padding * 2) + padding, y: height - padding - Math.min(1, d.players / 100) * (height - padding * 2) }));

  const pathFrom = (pts: { x: number; y: number }[]) => {
    if (pts.length === 0) return "";
    return pts.map((p, i) => `${i === 0 ? "M" : "L"}${p.x.toFixed(2)},${p.y.toFixed(2)}`).join(" ");
  };

  const cpuPath = pathFrom(pointsCpu);
  const ramPath = pathFrom(pointsRam);

  return (
    <svg viewBox={`0 0 ${width} ${height}`} width="100%" height="100%" preserveAspectRatio="none">
      <defs>
        <linearGradient id="g1" x1="0" x2="1">
          <stop offset="0%" stopColor="#8b5cf6" stopOpacity="0.7" />
          <stop offset="100%" stopColor="#7c3aed" stopOpacity="0.2" />
        </linearGradient>
        <linearGradient id="g2" x1="0" x2="1">
          <stop offset="0%" stopColor="#c084fc" stopOpacity="0.7" />
          <stop offset="100%" stopColor="#6d28d9" stopOpacity="0.2" />
        </linearGradient>
      </defs>

      {/* background grid */}
      <g stroke="rgba(255,255,255,0.03)" strokeWidth={1}>
        {[0, 0.25, 0.5, 0.75, 1].map((v, i) => (
          <line key={i} x1={padding} x2={width - padding} y1={padding + v * (height - padding * 2)} y2={padding + v * (height - padding * 2)} />
        ))}
      </g>

      {/* CPU path (animated) */}
      <motion.path d={cpuPath} fill="none" stroke="url(#g1)" strokeWidth={2.5} strokeLinecap="round" strokeLinejoin="round" initial={{ pathLength: 0 }} animate={{ pathLength: 1 }} transition={{ duration: 0.9 }} />

      {/* RAM path (animated) */}
      <motion.path d={ramPath} fill="none" stroke="url(#g2)" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" initial={{ pathLength: 0 }} animate={{ pathLength: 1 }} transition={{ duration: 1.0, delay: 0.08 }} />

      {/* players as small area under curve */}
      {pointsPlayers.length > 0 && (
        <path
          d={`${pointsPlayers.map((p, i) => `${i === 0 ? "M" : "L"}${p.x.toFixed(2)},${p.y.toFixed(2)}`).join(" ")} L ${padding},${height - padding} L ${width - padding},${height - padding} Z`}
          fill="rgba(124,58,237,0.06)"
        />
      )}

      {/* small dots at latest values (pulse) */}
      {pointsCpu.length > 0 && (
        <motion.circle cx={pointsCpu[pointsCpu.length - 1].x} cy={pointsCpu[pointsCpu.length - 1].y} r={3.5} fill="#8b5cf6" animate={{ scale: [1, 1.6, 1] }} transition={{ duration: 1.7, repeat: Infinity }} />
      )}
      {pointsRam.length > 0 && (
        <motion.circle cx={pointsRam[pointsRam.length - 1].x} cy={pointsRam[pointsRam.length - 1].y} r={3} fill="#c084fc" animate={{ scale: [1, 1.4, 1] }} transition={{ duration: 1.8, repeat: Infinity, delay: 0.3 }} />
      )}

      {/* labels */}
      <text x={padding} y={padding + 12} fontSize={10} fill="#b794f4">
        CPU
      </text>
      <text x={padding + 36} y={padding + 12} fontSize={10} fill="#e9d5ff">
        RAM
      </text>
    </svg>
  );
}
