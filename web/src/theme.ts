import { applyTheme } from "@cloudscape-design/components/theming"

const ink = "#111111"
const paper = "#ffffff"
const pencil = "#6b6b6b"

export function applyWireframeTheme() {
  applyTheme({
    theme: {
      tokens: {
        fontFamilyBase: '"Comic Neue", "Chalkboard SE", "Comic Sans MS", "Segoe Print", cursive',
        colorBackgroundLayoutMain: paper,
        colorBackgroundContainerContent: paper,
        colorBackgroundContainerHeader: paper,
        colorTextBodyDefault: ink,
        colorTextBodySecondary: pencil,
        colorTextHeadingDefault: ink,
        colorTextLinkDefault: ink,
        colorTextLinkHover: ink,
        colorBorderDividerDefault: ink,
        colorBorderDividerSecondary: pencil,
        colorBackgroundButtonPrimaryDefault: paper,
        colorBackgroundButtonPrimaryHover: "#eeeeee",
        colorBackgroundButtonPrimaryActive: "#dddddd",
        colorTextButtonPrimaryDefault: ink,
        colorTextButtonPrimaryHover: ink,
        colorTextButtonPrimaryActive: ink,
        colorBorderButtonPrimaryDefault: ink,
        colorBorderButtonPrimaryHover: ink,
        colorBorderButtonPrimaryActive: ink,
        colorBackgroundButtonNormalHover: "#eeeeee",
        colorBackgroundButtonNormalActive: "#dddddd",
        colorBorderButtonNormalDefault: ink,
        colorBorderButtonNormalHover: ink,
        colorBorderButtonNormalActive: ink,
        colorTextButtonNormalDefault: ink,
        colorTextButtonNormalHover: ink,
        colorTextButtonNormalActive: ink,
        colorBackgroundControlChecked: ink,
        colorBorderInputDefault: ink,
        colorBorderInputFocused: ink,
        colorBorderItemFocused: ink,
        borderRadiusButton: "2px",
        borderRadiusContainer: "2px",
        borderRadiusInput: "2px",
      },
    },
  })
}
