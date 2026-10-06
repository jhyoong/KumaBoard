import { Link } from 'react-router'
import type { Device } from '../api'
import { StateBadge } from './StateBadge'

// DeviceRail lists every device beside the detail view so wide screens can
// switch device without going back to the grid. Hidden below the 3xl tier.
export function DeviceRail({ devices, current }: { devices: Device[]; current: string }) {
  return (
    <nav aria-label="Devices" className="hidden 3xl:sticky 3xl:top-6 3xl:block 3xl:w-[18rem] 3xl:shrink-0 3xl:self-start">
      <ul className="3xl:space-y-1">
        {devices.map((d) => (
          <li key={d.name}>
            <Link
              to={`/devices/${d.name}`}
              aria-current={d.name === current ? 'page' : undefined}
              className={`3xl:flex 3xl:items-center 3xl:justify-between 3xl:gap-2 3xl:rounded 3xl:border 3xl:px-3 3xl:py-2 3xl:text-sm ${
                d.name === current
                  ? '3xl:border-border-strong 3xl:bg-surface 3xl:font-medium 3xl:text-fg'
                  : '3xl:border-transparent 3xl:text-fg-muted 3xl:hover:bg-surface-muted 3xl:hover:text-fg'
              }`}
            >
              <span className="3xl:truncate">{d.name}</span>
              <StateBadge state={d.state} />
            </Link>
          </li>
        ))}
      </ul>
    </nav>
  )
}
