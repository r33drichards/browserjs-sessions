import { Route, Routes } from "react-router-dom"
import { SessionDetail } from "./pages/SessionDetail"
import { SessionsList } from "./pages/SessionsList"

export function App() {
  return (
    <Routes>
      <Route path="/" element={<SessionsList />} />
      <Route path="/sessions/:id" element={<SessionDetail />} />
    </Routes>
  )
}
