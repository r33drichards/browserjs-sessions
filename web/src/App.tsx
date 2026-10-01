import { Route, Routes, useParams } from "react-router-dom"
import { SessionDetail } from "./pages/SessionDetail"
import { SessionsList } from "./pages/SessionsList"

// Keyed on the id so moving between two sessions starts from clean page state.
function SessionDetailRoute() {
  const { id = "" } = useParams()
  return <SessionDetail key={id} id={id} />
}

export function App() {
  return (
    <Routes>
      <Route path="/" element={<SessionsList />} />
      <Route path="/sessions/:id" element={<SessionDetailRoute />} />
    </Routes>
  )
}
