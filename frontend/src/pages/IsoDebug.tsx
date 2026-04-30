import { useEffect, useRef, useState } from 'react'
import {
  createIsoDebugScene,
  type ClickedStationInfo,
  type IsoDebugSceneHandle,
} from '../factory/iso-debug'

// Visual sandbox for the 3D rewrite (SKY-196 / SKY-197). Mounts a
// Babylon scene with a floor grid + one station. Default camera is
// top-down ortho. Babylon's ArcRotateCamera handles input directly:
// LMB-drag = orbit, RMB-drag (or ctrl+LMB) = pan, wheel = zoom. The
// Reset button snaps back to the initial top-down view.

export default function IsoDebug() {
  const containerRef = useRef<HTMLDivElement>(null)
  const sceneRef = useRef<IsoDebugSceneHandle | null>(null)
  const [picked, setPicked] = useState<ClickedStationInfo | null>(null)

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let cancelled = false
    let unsubClick: (() => void) | null = null
    createIsoDebugScene(container).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      sceneRef.current = scene
      unsubClick = scene.onStationClick(setPicked)
    })
    return () => {
      cancelled = true
      unsubClick?.()
      sceneRef.current?.destroy()
      sceneRef.current = null
    }
  }, [])

  // Esc closes the drawer — common video-game-y dismiss gesture.
  useEffect(() => {
    if (!picked) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPicked(null)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [picked])

  return (
    <div className="relative -mx-8 -my-8">
      <div
        ref={containerRef}
        className="relative w-full overflow-hidden"
        style={{ height: 'calc(100vh - 69px)' }}
      />
      <button
        type="button"
        onClick={() => sceneRef.current?.resetView()}
        className="absolute bottom-4 right-4 rounded-md bg-white/92 px-3 py-2 text-[11px] font-semibold text-text-primary shadow transition hover:bg-white"
      >
        Reset view
      </button>
      <StationDrawer info={picked} />
    </div>
  )
}

// Bottom slide-up sheet — top-down view of the clicked station as
// pure HTML. Reads as the station's chassis seen from above with
// two recessed trays (intake left, main right), each ringed by a
// cyan LED glow and a dark machined floor. Mirrors the 3D scene's
// material palette so the drawer feels like a HUD readout of the
// thing on screen, not a generic data panel.
function StationDrawer({ info }: { info: ClickedStationInfo | null }) {
  const open = info != null
  return (
    <div
      className={`pointer-events-none absolute inset-x-0 bottom-0 z-40 transition-transform duration-300 ease-out ${
        open ? 'translate-y-0' : 'translate-y-full'
      }`}
      style={{ height: '46vh' }}
      aria-hidden={!open}
    >
      <div className="pointer-events-auto relative h-full bg-surface-raised/95 backdrop-blur-xl border-t border-border-glass shadow-2xl shadow-black/[0.12] flex items-stretch p-5">
        <StationChassis info={info} />
      </div>
    </div>
  )
}

// Cream chassis carrying the two trays. Background color, rounded
// edges, and inner padding mimic the 3D station body's silhouette
// at a higher zoom. The trays inside are inset boxes with cyan LED
// borders; the cream surrounding them carries the same warm
// off-white as the station body material (`#ece6d8`).
function StationChassis({ info }: { info: ClickedStationInfo | null }) {
  const queueCount = info?.queuedCount ?? 0
  const runCount = info?.runCount ?? 0
  return (
    <div
      className="relative flex w-full gap-4 rounded-2xl p-4"
      style={{
        background: 'linear-gradient(180deg, #f1ebdc 0%, #e6e0d2 100%)',
        boxShadow:
          'inset 0 1px 0 rgba(255,255,255,0.8), inset 0 -2px 0 rgba(0,0,0,0.06), 0 4px 16px rgba(0,0,0,0.05)',
      }}
    >
      <Tray
        label="Queue"
        accent="#ff9c3a"
        widthClass="w-[24%]"
        emptyMessage="Idle — no entities waiting"
        count={queueCount}
        chipColor="#ff9c3a"
        renderItem={(i) => (
          <span className="font-mono text-[11px] text-white/80">entity-{i + 1}</span>
        )}
      />
      <Tray
        label={info?.label ?? '—'}
        accent="#7cf7ec"
        widthClass="flex-1"
        emptyMessage="No runs in flight"
        count={runCount}
        chipColor="#7aa3ff"
        renderItem={(i) => (
          <div className="flex w-full items-baseline justify-between gap-3">
            <span className="font-mono text-[11px] text-white/85">run-{i + 1}</span>
            <span className="font-mono text-[10px] text-white/50">2m 14s · $0.18</span>
          </div>
        )}
      />
    </div>
  )
}

// One inset tray panel — dark machined floor, cyan LED ring around
// the rim, etched header at the top. The LED ring is two
// box-shadows: a tight 1px line (the trim itself) and a soft outer
// glow (the bloom). Combines with a dark interior to read like the
// 3D tray opening seen from above.
function Tray({
  label,
  accent,
  widthClass,
  count,
  chipColor,
  emptyMessage,
  renderItem,
}: {
  label: string
  accent: string
  widthClass: string
  count: number
  chipColor: string
  emptyMessage: string
  renderItem: (index: number) => React.ReactNode
}) {
  return (
    <div
      className={`relative flex flex-col rounded-xl ${widthClass}`}
      style={{
        background: 'linear-gradient(180deg, #14120d 0%, #0d0c08 100%)',
        boxShadow: `
          inset 0 0 0 1px ${hexToRgba(accent, 0.55)},
          inset 0 1px 0 rgba(255,255,255,0.04),
          0 0 0 2px ${hexToRgba(accent, 0.12)},
          0 0 18px ${hexToRgba(accent, 0.28)}
        `,
      }}
    >
      <header
        className="px-4 py-2.5 border-b text-center"
        style={{ borderColor: hexToRgba(accent, 0.22) }}
      >
        <span
          className="text-[12px] font-semibold uppercase tracking-[0.18em]"
          style={{
            color: '#ffffff',
            textShadow: `0 0 10px ${hexToRgba(accent, 0.65)}, 0 0 2px rgba(255,255,255,0.6)`,
          }}
        >
          {label}
        </span>
      </header>
      <ul className="flex-1 overflow-y-auto px-3 py-3 flex flex-col gap-1.5">
        {count === 0 ? (
          <li className="px-2 py-1 text-[11px] italic text-white/35">{emptyMessage}</li>
        ) : (
          Array.from({ length: count }).map((_, i) => (
            <li
              key={i}
              className="flex items-center gap-2.5 rounded-md px-2.5 py-1.5"
              style={{
                background: 'rgba(255,255,255,0.04)',
                boxShadow: `inset 0 0 0 1px ${hexToRgba(chipColor, 0.18)}`,
              }}
            >
              <span
                aria-hidden
                className="inline-block h-1.5 w-1.5 rounded-full"
                style={{
                  background: chipColor,
                  boxShadow: `0 0 6px ${chipColor}`,
                }}
              />
              {renderItem(i)}
            </li>
          ))
        )}
      </ul>
    </div>
  )
}

function hexToRgba(hex: string, alpha: number): string {
  const h = hex.replace('#', '')
  const r = parseInt(h.slice(0, 2), 16)
  const g = parseInt(h.slice(2, 4), 16)
  const b = parseInt(h.slice(4, 6), 16)
  return `rgba(${r}, ${g}, ${b}, ${alpha})`
}
