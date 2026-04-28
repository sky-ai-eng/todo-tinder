import { useEffect, useRef, useState } from 'react'
import {
  createIsoDebugScene,
  type CameraStateForHUD,
  type IsoDebugSceneHandle,
} from '../factory/iso-debug'

// Stage-0 visual sandbox for the 3D rewrite (SKY-196). Mounts a tiny
// Pixi scene with a floor grid, axis indicator, and a few test cubes.
// Default camera state is top-down (flat). Drag to pan, shift+drag to
// rotate, wheel to zoom. Reset button snaps back to flat. Toggle button
// switches between orthographic and perspective projection.

const radToDeg = (r: number) => Math.round((r * 180) / Math.PI)

export default function IsoDebug() {
  const containerRef = useRef<HTMLDivElement>(null)
  const sceneRef = useRef<IsoDebugSceneHandle | null>(null)
  const [cam, setCam] = useState<CameraStateForHUD>({
    pitch: 0,
    yaw: 0,
    zoom: 1,
    mode: 'orthographic',
  })

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let cancelled = false
    let unsubCam: (() => void) | null = null
    createIsoDebugScene(container).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      sceneRef.current = scene
      unsubCam = scene.onCameraChange(setCam)
    })
    return () => {
      cancelled = true
      unsubCam?.()
      sceneRef.current?.destroy()
      sceneRef.current = null
    }
  }, [])

  return (
    <div className="relative -mx-8 -my-8">
      <div
        ref={containerRef}
        className="relative w-full overflow-hidden"
        style={{ height: 'calc(100vh - 69px)' }}
      />
      <div className="absolute bottom-4 right-4 flex items-end gap-2">
        <div className="rounded-md bg-white/92 px-3 py-2 font-mono text-[11px] text-text-secondary shadow">
          <div className="font-semibold text-text-primary">SKY-196 — projection foundation</div>
          <div>pitch: {radToDeg(cam.pitch)}°</div>
          <div>yaw: {radToDeg(cam.yaw)}°</div>
          <div>zoom: {cam.zoom.toFixed(2)}x</div>
          <div>mode: {cam.mode}</div>
          <div className="mt-1 border-t border-black/10 pt-1 text-[10px] text-text-tertiary">
            drag · shift+drag · wheel
          </div>
        </div>
        <div className="flex flex-col gap-2">
          <button
            type="button"
            onClick={() => sceneRef.current?.toggleMode()}
            className="rounded-md bg-white/92 px-3 py-2 text-[11px] font-semibold text-text-primary shadow transition hover:bg-white"
          >
            {cam.mode === 'orthographic' ? 'Perspective →' : '← Orthographic'}
          </button>
          <button
            type="button"
            onClick={() => sceneRef.current?.resetView()}
            className="rounded-md bg-white/92 px-3 py-2 text-[11px] font-semibold text-text-primary shadow transition hover:bg-white"
          >
            Reset view
          </button>
        </div>
      </div>
    </div>
  )
}
