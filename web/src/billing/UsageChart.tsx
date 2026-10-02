// Charges by day, awake over disk, in the wireframe's ink: solid for awake,
// hatched for disk, so the two are told apart without colour. The table of
// sessions beneath it on the page is the same period in numbers.
import { useState } from "react"
import type { Usage } from "../billingApi"
import { dollars, duration, shortDate } from "../billingApi"

const HEIGHT = 140

export function UsageChart({ days }: { days: Usage["days"] }) {
  const [hover, setHover] = useState<number | null>(null)
  const totals = days.map(d => (d.awakeMicros ?? 0) + (d.diskMicros ?? 0))
  const max = Math.max(...totals, 1)
  const shown = hover === null ? null : days[hover]

  if (days.length === 0) return <p className="wf-note">Nothing was used in this period.</p>

  return (
    <figure className="wf-chart" aria-label={`Charges by day, ${shortDate(days[0].date)} to ${shortDate(days[days.length - 1].date)}`}>
      <div className="wf-chart-legend">
        <span>
          <i className="wf-chart-key wf-chart-awake" /> Awake
        </span>
        <span>
          <i className="wf-chart-key wf-chart-disk" /> Disk
        </span>
        <span className="wf-note" aria-live="polite">
          {shown
            ? `${shortDate(shown.date)}: awake ${dollars(shown.awakeMicros ?? 0)} (${duration(shown.awakeSeconds ?? 0)}), disk ${dollars(shown.diskMicros ?? 0)}`
            : `most in a day: ${dollars(max)}`}
        </span>
      </div>
      {/* As wide as its days need, so a short period is not stretched into slabs. */}
      <div style={{ maxWidth: days.length * 30 }}>
      <div className="wf-chart-plot" style={{ height: HEIGHT }} onMouseLeave={() => setHover(null)}>
        {days.map((d, i) => (
          <div
            key={d.date}
            className="wf-chart-day"
            data-hover={hover === i || undefined}
            tabIndex={0}
            role="img"
            aria-label={`${shortDate(d.date)}: awake ${dollars(d.awakeMicros ?? 0)}, disk ${dollars(d.diskMicros ?? 0)}`}
            onMouseEnter={() => setHover(i)}
            onFocus={() => setHover(i)}
            onBlur={() => setHover(null)}
          >
            <div className="wf-chart-disk" style={{ height: ((d.diskMicros ?? 0) / max) * HEIGHT }} />
            <div className="wf-chart-awake" style={{ height: ((d.awakeMicros ?? 0) / max) * HEIGHT }} />
          </div>
        ))}
      </div>
      <div className="wf-chart-axis wf-note">
        <span>{shortDate(days[0].date)}</span>
        <span>{shortDate(days[days.length - 1].date)}</span>
      </div>
      </div>
    </figure>
  )
}
