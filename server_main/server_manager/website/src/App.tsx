// App.jsx
import { useState } from "react";
import Topbar from "./comp/Topbar";
import Instances from "./comp/Instances";
import MinecraftProxyDashboard from "./comp/MinecraftProxyDashboard";

export default function App() {
  const [active, setActive] = useState("dashboard");

  return (
    <div className="flex flex-col h-screen bg-[#0d0714] text-white">
      <Topbar active={active} setActive={setActive} />

      <main className="flex-1 relative overflow-hidden">
        {/* Wrapper für Transitionen */}
        <div className="absolute inset-0">
          <div
            className={`absolute inset-0 transition-all duration-300 transform ${
              active === "dashboard"
                ? "opacity-100 translate-y-0 z-10"
                : "opacity-0 -translate-y-2 z-0 pointer-events-none"
            }`}
          >
            <MinecraftProxyDashboard />
          </div>

          <div
            className={`p-6 relative absolute inset-0 transition-all duration-300 transform ${
              active === "instances"
                ? "opacity-100 translate-y-0 z-10"
                : "opacity-0 -translate-y-2 z-0 pointer-events-none"
            }`}
          >
            <Instances />
          </div>
        </div>
      </main>
    </div>
  );
}