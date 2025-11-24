type TopbarProps = {
  active: string;
  setActive: React.Dispatch<React.SetStateAction<string>>;
};

export default function Topbar({ active, setActive }: TopbarProps) {
  const linkBase = "relative px-4 py-2 cursor-pointer text-white opacity-80 hover:opacity-100 transition";
  return (
    <div className="w-screen px-6 py-4 bg-transparent shadow-lg text-white flex justify-center">
      <div className="flex gap-8 text-lg font-semibold">
        <div onClick={() => setActive("dashboard")} className={linkBase}>
          Dashboard
          <span className={`absolute left-0 right-0 -bottom-1 h-[3px] bg-white rounded-full transition-transform duration-300 origin-center ${active === "dashboard" ? "scale-x-100" : "scale-x-0"}`} />
        </div>

        <div onClick={() => setActive("instances")} className={linkBase}>
          Instances
          <span className={`absolute left-0 right-0 -bottom-1 h-[3px] bg-white rounded-full transition-transform duration-300 origin-center ${active === "instances" ? "scale-x-100" : "scale-x-0"}`} />
        </div>
      </div>
    </div>
  );
}