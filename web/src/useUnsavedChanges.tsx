import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Modal from "@cloudscape-design/components/modal"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useCallback, useEffect, useRef } from "react"
import { useBlocker } from "react-router-dom"

// Cloudscape's "communicating unsaved changes": leaving a page with changes
// (a link, Cancel, the back button) raises the Leave page modal, and the
// browser's own navigation (close, reload) its native prompt. With nothing
// changed there is no risk of loss and no modal.
export function useUnsavedChanges(dirty: boolean) {
  // Set for the navigation that follows a successful save: nothing is lost by it.
  const saved = useRef(false)
  const blocker = useBlocker(useCallback(() => dirty && !saved.current, [dirty]))

  useEffect(() => {
    if (!dirty) return
    const warn = (e: BeforeUnloadEvent) => {
      if (saved.current) return
      e.preventDefault()
      e.returnValue = "" // the browser shows its own text
    }
    window.addEventListener("beforeunload", warn)
    return () => window.removeEventListener("beforeunload", warn)
  }, [dirty])

  const blocked = blocker.state === "blocked"
  // Cloudscape keeps a hidden modal's content in the page; this one exists only while asked.
  const modal = blocked && (
    <Modal
      visible
      onDismiss={() => blocker.reset?.()}
      header="Leave page"
      closeAriaLabel="Close"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={() => blocker.reset?.()}>
              Cancel
            </Button>
            <Button variant="primary" onClick={() => blocker.proceed?.()}>
              Leave
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      Are you sure that you want to leave the current page? The changes that you made won&apos;t be saved.
    </Modal>
  )

  return {
    modal,
    // Call before navigating away from a page whose work has just been saved.
    markSaved: () => {
      saved.current = true
    },
  }
}
