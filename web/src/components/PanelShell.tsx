// The shell for a page with a split panel: the create page writes a policy in
// one. The panel belongs to Cloudscape's app layout; the app's own header
// stays above it.
import AppLayout from "@cloudscape-design/components/app-layout"
import type { Crumb } from "../shell"
import { ShellBody, ShellHeader } from "../shell"

interface Props {
  children: React.ReactNode
  breadcrumbs?: Crumb[]
  panel: React.ReactNode // a <SplitPanel>
  open: boolean
  onToggle: (open: boolean) => void
}

export function PanelShell({ children, breadcrumbs, panel, open, onToggle }: Props) {
  return (
    <>
      <ShellHeader />
      <AppLayout
        headerSelector=".wf-header"
        navigationHide
        toolsHide
        contentType="form"
        content={<ShellBody breadcrumbs={breadcrumbs}>{children}</ShellBody>}
        splitPanel={panel}
        splitPanelOpen={open}
        onSplitPanelToggle={e => onToggle(e.detail.open)}
        splitPanelPreferences={{ position: "bottom" }}
        onSplitPanelPreferencesChange={() => {}}
        ariaLabels={{ notifications: "Notifications" }}
      />
    </>
  )
}
