import { Suspense, lazy } from "react"
import { Route, Routes, useParams } from "react-router-dom"
import { SessionDetail } from "./pages/SessionDetail"
import { SessionsList } from "./pages/SessionsList"

// The pages around the list and the session load when first visited, so the
// two everyone opens stay as small as they were.
const CreateSession = lazy(() => import("./pages/CreateSession").then(m => ({ default: m.CreateSession })))
const CreateToken = lazy(() => import("./pages/CreateToken").then(m => ({ default: m.CreateToken })))
const EditPolicy = lazy(() => import("./pages/EditPolicy").then(m => ({ default: m.EditPolicy })))
const Tokens = lazy(() => import("./pages/Tokens").then(m => ({ default: m.Tokens })))

// Keyed on the id so moving between two sessions starts from clean page state.
function SessionDetailRoute() {
  const { id = "" } = useParams()
  return <SessionDetail key={id} id={id} />
}

function EditPolicyRoute() {
  const { id = "" } = useParams()
  return <EditPolicy key={id} id={id} />
}

export function App() {
  return (
    <Suspense fallback={null}>
    <Routes>
      <Route path="/" element={<SessionsList />} />
      {/* "create" is not a session id (isSessionId), so it cannot shadow one. */}
      <Route path="/sessions/create" element={<CreateSession />} />
      <Route path="/sessions/:id" element={<SessionDetailRoute />} />
      <Route path="/sessions/:id/policy/edit" element={<EditPolicyRoute />} />
      <Route path="/tokens" element={<Tokens />} />
      <Route path="/tokens/create" element={<CreateToken />} />
    </Routes>
    </Suspense>
  )
}
