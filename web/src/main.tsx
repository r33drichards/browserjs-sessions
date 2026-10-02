import "@cloudscape-design/global-styles/index.css"
import React from "react"
import ReactDOM from "react-dom/client"
import { RouterProvider, createBrowserRouter } from "react-router-dom"
import { App } from "./App"
import { MeProvider } from "./auth/MeProvider"
import { applyWireframeTheme } from "./theme"
import "./wireframe.css"

applyWireframeTheme()

// A data router, so pages with unsaved work can hold a navigation (useBlocker).
// The routes themselves are in App.
const router = createBrowserRouter([{ path: "*", element: <App /> }])

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <MeProvider>
      <RouterProvider router={router} />
    </MeProvider>
  </React.StrictMode>,
)
