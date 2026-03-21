import { Routes, Route } from "react-router-dom";
import { PilotStatusWidget } from "./components/pilot-status";
import { PilotConfigPage } from "./pages/PilotConfig";

function App() {
  return (
    <div className="h-screen w-screen overflow-hidden">
      <Routes>
        <Route path="/" element={<PilotStatusWidget />} />
        <Route path="/settings" element={<PilotConfigPage />} />
      </Routes>
    </div>
  );
}

export default App;
