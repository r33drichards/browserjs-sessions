import "@cloudscape-design/global-styles/index.css"
import React from "react"
import ReactDOM from "react-dom/client"
import { BrowserRouter } from "react-router-dom"
import { App } from "./App"
import { MeProvider } from "./auth/MeProvider"
import { applyWireframeTheme } from "./theme"
import "./wireframe.css"

applyWireframeTheme()

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <MeProvider>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </MeProvider>
  </React.StrictMode>,
)
