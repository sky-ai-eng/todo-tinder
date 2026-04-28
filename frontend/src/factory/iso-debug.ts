// Stage-0 debug scene for the 3D rewrite (SKY-196).
//
// Renders a floor grid + a few test cubes through a Camera. Defaults to
// top-down (camera flat on the floor, content axis-aligned, no shear).
// Drag pans, shift+drag rotates, wheel zooms, /factory-iso-debug HUD has
// a Reset button to snap back to flat.
//
// Validates:
//   - At pitch=0/yaw=0 the scene reads as the existing 2D layout.
//   - Tilting reveals top + front + right faces of cubes; floor cells
//     shear into parallelograms (no longer axis-aligned, expected).
//   - Reset returns to flat.
//   - Pan + zoom + rotate compose without drift.

import { Application, Container, Graphics, Text } from 'pixi.js'
import { Camera, type ProjectionMode, type Vec2, type Vec3 } from './projection'

const FLOOR_SIZE = 1200
const FLOOR_CELL = 120

interface Cube {
  /** World position of the bottom-back-left corner. */
  x: number
  y: number
  z: number
  /** Extents along world x/y/z respectively. */
  w: number
  d: number
  h: number
  color: number
  label?: string
}

const TEST_CUBES: Cube[] = [
  { x: 0, y: 0, z: 0, w: 120, d: 120, h: 80, color: 0xc47a5a, label: '(0, 0, 0) 80h' },
  { x: 240, y: 240, z: 0, w: 120, d: 120, h: 80, color: 0x7a9aad, label: '(240, 240, 0) 80h' },
  { x: 600, y: 600, z: 0, w: 120, d: 120, h: 160, color: 0x6ea87a, label: '(600, 600, 0) 160h' },
  { x: 960, y: 240, z: 0, w: 120, d: 120, h: 40, color: 0xb8943a, label: '(960, 240, 0) 40h' },
  { x: 240, y: 840, z: 0, w: 200, d: 200, h: 60, color: 0x9a7aad, label: '(240, 840, 0) wider' },
]

export interface CameraStateForHUD {
  pitch: number
  yaw: number
  zoom: number
  mode: ProjectionMode
}

export interface IsoDebugSceneHandle {
  destroy: () => void
  resetView: () => void
  toggleMode: () => void
  /** Subscribe to camera state changes. The HUD uses this to render
   * pitch/yaw/zoom/mode live. Returns an unsubscribe function. */
  onCameraChange: (cb: (s: CameraStateForHUD) => void) => () => void
}

export async function createIsoDebugScene(container: HTMLDivElement): Promise<IsoDebugSceneHandle> {
  const app = new Application()
  await app.init({
    resizeTo: container,
    background: 0xf7f5f2,
    antialias: true,
    resolution: window.devicePixelRatio || 1,
    autoDensity: true,
  })
  container.appendChild(app.canvas)
  // Hoisted up from the gesture-handler block so syncFocalToCenter (which
  // needs the canvas for getBoundingClientRect) can be defined before
  // the initial centerCamera() call below.
  const canvas = app.canvas

  const camera = new Camera()

  // Keep the perspective focal point glued to whatever world point is
  // currently at screen center. Recomputed via orthographic math
  // (independent of focal) to avoid the obvious circular dependency.
  // Without this sync, perspective foreshortens around world origin
  // and the scene appears to shrink toward the wrong vanishing point
  // as the user pans/zooms.
  const syncFocalToCenter = () => {
    const rect = canvas.getBoundingClientRect()
    const center: Vec2 = { x: rect.width / 2, y: rect.height / 2 }
    camera.setFocalPoint(camera.orthographicScreenToFloor(center))
  }

  // Center the world on screen with a 1.0 default zoom. The scene's
  // logical center sits at (FLOOR_SIZE/2, FLOOR_SIZE/2) in world coords;
  // panning centers that on screen.
  const centerCamera = () => {
    const w = app.screen.width
    const h = app.screen.height
    camera.setPan(w / 2 - FLOOR_SIZE / 2, h / 2 - FLOOR_SIZE / 2)
    syncFocalToCenter()
  }
  centerCamera()

  // Layered rendering — floor grid behind the cubes, axis indicator in
  // its own layer so we can z-sort cubes without touching the grid.
  const root = new Container()
  app.stage.addChild(root)
  const gridLayer = new Container()
  const axisLayer = new Container()
  const cubesLayer = new Container()
  root.addChild(gridLayer)
  root.addChild(axisLayer)
  root.addChild(cubesLayer)

  // Dirty-flag pattern: rebuild geometry on the next tick after camera
  // changes. Avoids re-projecting on every redundant pointer event when
  // multiple fire in the same frame.
  let dirty = true
  const unsubscribeCamera = camera.onChange(() => {
    dirty = true
  })

  const rebuild = () => {
    gridLayer.removeChildren().forEach((c) => c.destroy({ children: true }))
    axisLayer.removeChildren().forEach((c) => c.destroy({ children: true }))
    cubesLayer.removeChildren().forEach((c) => c.destroy({ children: true }))
    drawFloorGrid(gridLayer, camera)
    drawAxisIndicator(axisLayer, camera)
    // Painter's order: cubes drawn back-to-front so closer ones occlude
    // farther ones. After rotation, "back" depends on yaw; sort by the
    // projected screen y of each cube's centroid (smaller y = farther
    // back, drawn first).
    const sorted = [...TEST_CUBES]
      .map((c) => ({
        cube: c,
        cy: camera.worldToScreen({ x: c.x + c.w / 2, y: c.y + c.d / 2, z: 0 }).y,
      }))
      .sort((a, b) => a.cy - b.cy)
    for (const { cube } of sorted) drawCube(cubesLayer, camera, cube)
  }

  rebuild()
  dirty = false

  app.ticker.add(() => {
    if (dirty) {
      rebuild()
      dirty = false
    }
  })

  // Gesture handlers attached to the canvas. We use raw DOM events
  // rather than pixi's interaction system because we want unconditional
  // capture (drag + rotate) on the canvas without competing with future
  // hit-tested object interactions. (`canvas` was hoisted above so the
  // syncFocalToCenter helper could see it.)
  canvas.style.touchAction = 'none' // suppress browser pan/zoom on touch

  let dragging = false
  let lastClientX = 0
  let lastClientY = 0
  let rotating = false
  // Pivot for the in-progress rotation: the floor point under the screen
  // center at drag-start, locked there throughout the drag. The viewer's
  // attention naturally sits at the center of the viewport, so rotations
  // around that point read more intuitively than rotations around
  // wherever the cursor happens to be. (Wheel zoom still anchors on the
  // cursor — different gesture, different intent: zoom-to-focus-point.)
  let rotatePivotScreen: Vec2 = { x: 0, y: 0 }
  let rotatePivotWorld: Vec3 | null = null

  // Translate a viewport-space pointer event into canvas-local coords so
  // the camera math (which is in canvas-local space) stays consistent.
  const canvasPoint = (e: PointerEvent | WheelEvent): Vec2 => {
    const rect = canvas.getBoundingClientRect()
    return { x: e.clientX - rect.left, y: e.clientY - rect.top }
  }

  // After a rotation or zoom, repin a world point to a screen position
  // by adjusting the pan. Foundation of "rotate around cursor" and
  // "zoom to cursor" — both keep a chosen world point glued under a
  // chosen screen point.
  const repin = (worldPoint: Vec3, screenAnchor: Vec2) => {
    const after = camera.worldToScreen(worldPoint)
    camera.pan(screenAnchor.x - after.x, screenAnchor.y - after.y)
  }

  const onPointerDown = (e: PointerEvent) => {
    dragging = true
    lastClientX = e.clientX
    lastClientY = e.clientY
    rotating = e.shiftKey
    if (rotating) {
      const rect = canvas.getBoundingClientRect()
      rotatePivotScreen = { x: rect.width / 2, y: rect.height / 2 }
      rotatePivotWorld = camera.orthographicScreenToFloor(rotatePivotScreen)
      // Anchor perspective foreshortening on the rotation pivot so the
      // scene rotates around the user's focal point instead of around
      // world origin.
      camera.setFocalPoint(rotatePivotWorld)
    }
    canvas.setPointerCapture(e.pointerId)
  }
  const onPointerMove = (e: PointerEvent) => {
    if (!dragging) return
    const dx = e.clientX - lastClientX
    const dy = e.clientY - lastClientY
    lastClientX = e.clientX
    lastClientY = e.clientY
    if (rotating && rotatePivotWorld) {
      // Drag UP (negative dy) increases pitch — camera tilts forward,
      // exposing more of the front faces. Drag RIGHT (positive dx)
      // rotates the scene clockwise (camera orbits CCW around pivot,
      // matching Blender/Maya tumble conventions). Both axes use the
      // same rate: a ~600px drag completes a quarter turn.
      const yawDelta = (-dx / 600) * (Math.PI / 2)
      const pitchDelta = (-dy / 600) * (Math.PI / 2)
      camera.setYaw(camera.yaw + yawDelta)
      camera.setPitch(camera.pitch + pitchDelta)
      repin(rotatePivotWorld, rotatePivotScreen)
    } else {
      // Plain pan: do NOT resync the focal point. Moving the focal in
      // world space changes its depth-from-camera, which rescales every
      // other point in perspective mode and reads as "zooming toward
      // origin." Pan should be a pure translation of what's already
      // foreshortened — the focal stays put until the next rotation
      // explicitly moves it.
      camera.pan(dx, dy)
    }
  }
  const onPointerUp = (e: PointerEvent) => {
    dragging = false
    rotating = false
    rotatePivotWorld = null
    if (canvas.hasPointerCapture(e.pointerId)) canvas.releasePointerCapture(e.pointerId)
  }
  const onWheel = (e: WheelEvent) => {
    e.preventDefault()
    // Zoom-to-cursor: anchor the floor point under the cursor before
    // and after the zoom step, so the user's focal point doesn't drift.
    // Use the perspective-aware inverse here — in perspective mode the
    // world point under the cursor is at a slightly different (x, y)
    // than the ortho inverse, and using the wrong inverse makes the
    // anchor drift visibly (looks like the world origin is being
    // dragged toward the cursor as you zoom). The focal point doesn't
    // change during zoom, so screenToFloor's dependency on focal is
    // stable across the call.
    const screenAnchor = canvasPoint(e)
    const worldAnchor = camera.screenToFloor(screenAnchor)
    const factor = Math.exp(-e.deltaY * 0.001)
    camera.setZoom(camera.zoom * factor)
    repin(worldAnchor, screenAnchor)
  }

  canvas.addEventListener('pointerdown', onPointerDown)
  canvas.addEventListener('pointermove', onPointerMove)
  canvas.addEventListener('pointerup', onPointerUp)
  canvas.addEventListener('pointercancel', onPointerUp)
  canvas.addEventListener('wheel', onWheel, { passive: false })

  const ro = new ResizeObserver(() => {
    centerCamera()
  })
  ro.observe(container)

  return {
    destroy: () => {
      canvas.removeEventListener('pointerdown', onPointerDown)
      canvas.removeEventListener('pointermove', onPointerMove)
      canvas.removeEventListener('pointerup', onPointerUp)
      canvas.removeEventListener('pointercancel', onPointerUp)
      canvas.removeEventListener('wheel', onWheel)
      unsubscribeCamera()
      ro.disconnect()
      app.destroy(true, { children: true, texture: true })
    },
    resetView: () => {
      const w = app.screen.width
      const h = app.screen.height
      camera.reset(w / 2 - FLOOR_SIZE / 2, h / 2 - FLOOR_SIZE / 2)
      syncFocalToCenter()
    },
    toggleMode: () => {
      camera.toggleMode()
    },
    onCameraChange: (cb) => {
      // Throttle to one notification per animation frame — pointer
      // events fire much faster and the HUD doesn't need that resolution.
      let raf: number | null = null
      const snapshot = (): CameraStateForHUD => ({
        pitch: camera.pitch,
        yaw: camera.yaw,
        zoom: camera.zoom,
        mode: camera.mode,
      })
      const wrapped = () => {
        if (raf != null) return
        raf = requestAnimationFrame(() => {
          raf = null
          cb(snapshot())
        })
      }
      const unsub = camera.onChange(wrapped)
      // Fire once immediately so the HUD doesn't start blank.
      cb(snapshot())
      return () => {
        if (raf != null) cancelAnimationFrame(raf)
        unsub()
      }
    },
  }
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

function drawFloorGrid(parent: Container, camera: Camera) {
  // Axis-aligned grid in world. Becomes parallelogram on screen when
  // pitch>0 or yaw≠0. Subtle alpha so the grid reads as background.
  const grid = new Graphics()
  for (let i = 0; i <= FLOOR_SIZE; i += FLOOR_CELL) {
    const a = camera.worldToScreen({ x: 0, y: i, z: 0 })
    const b = camera.worldToScreen({ x: FLOOR_SIZE, y: i, z: 0 })
    grid.moveTo(a.x, a.y)
    grid.lineTo(b.x, b.y)
    const c = camera.worldToScreen({ x: i, y: 0, z: 0 })
    const d = camera.worldToScreen({ x: i, y: FLOOR_SIZE, z: 0 })
    grid.moveTo(c.x, c.y)
    grid.lineTo(d.x, d.y)
  }
  grid.stroke({ width: 1, color: 0x1a1a1a, alpha: 0.12 })
  parent.addChild(grid)
}

function drawAxisIndicator(parent: Container, camera: Camera) {
  // Three colored arrows from world origin. X=red, Y=green, Z=blue.
  const len = 200
  const o = camera.worldToScreen({ x: 0, y: 0, z: 0 })
  const xEnd = camera.worldToScreen({ x: len, y: 0, z: 0 })
  const yEnd = camera.worldToScreen({ x: 0, y: len, z: 0 })
  const zEnd = camera.worldToScreen({ x: 0, y: 0, z: len })

  const lines = new Graphics()
  lines.moveTo(o.x, o.y)
  lines.lineTo(xEnd.x, xEnd.y)
  lines.stroke({ width: 3, color: 0xd14a4a, alpha: 0.9 })
  lines.moveTo(o.x, o.y)
  lines.lineTo(yEnd.x, yEnd.y)
  lines.stroke({ width: 3, color: 0x4aa84a, alpha: 0.9 })
  lines.moveTo(o.x, o.y)
  lines.lineTo(zEnd.x, zEnd.y)
  lines.stroke({ width: 3, color: 0x4a6ad1, alpha: 0.9 })
  parent.addChild(lines)

  for (const [axis, end, color] of [
    ['X', xEnd, 0xd14a4a],
    ['Y', yEnd, 0x4aa84a],
    ['Z', zEnd, 0x4a6ad1],
  ] as const) {
    const t = new Text({
      text: axis,
      style: {
        fontFamily: 'Inter, system-ui, sans-serif',
        fontSize: 14,
        fontWeight: '700',
        fill: color,
      },
    })
    t.anchor.set(0.5, 0.5)
    t.x = end.x + (axis === 'X' ? 10 : 0)
    t.y = end.y + (axis === 'Y' ? 10 : axis === 'Z' ? -10 : 0)
    parent.addChild(t)
  }
}

function drawCube(parent: Container, camera: Camera, cube: Cube) {
  // Draw all six faces sorted back-to-front (painter's algorithm). For
  // any camera orientation the visible faces end up on top of the
  // hidden ones automatically — no separate back-face cull needed.
  // At pitch=0, side faces collapse to zero-height lines (h*sin(0)=0)
  // and the cube reads as a flat top rectangle; tilting reveals depth.
  // Each face gets a shade multiplier off the cube color to fake
  // directional lighting from above.
  const { x, y, z, w, d, h, color, label } = cube

  const project = (p: Vec3) => camera.worldToScreen(p)
  const bbl = project({ x: x, y: y, z: z })
  const bbr = project({ x: x + w, y: y, z: z })
  const bfr = project({ x: x + w, y: y + d, z: z })
  const bfl = project({ x: x, y: y + d, z: z })
  const tbl = project({ x: x, y: y, z: z + h })
  const tbr = project({ x: x + w, y: y, z: z + h })
  const tfr = project({ x: x + w, y: y + d, z: z + h })
  const tfl = project({ x: x, y: y + d, z: z + h })

  // Drop shadow on the floor — same xy footprint, slightly outset.
  const sBL = project({ x: x - 4, y: y - 4, z: 0 })
  const sBR = project({ x: x + w + 4, y: y - 4, z: 0 })
  const sFR = project({ x: x + w + 4, y: y + d + 4, z: 0 })
  const sFL = project({ x: x - 4, y: y + d + 4, z: 0 })
  const shadow = new Graphics()
  shadow.moveTo(sBL.x, sBL.y)
  shadow.lineTo(sBR.x, sBR.y)
  shadow.lineTo(sFR.x, sFR.y)
  shadow.lineTo(sFL.x, sFL.y)
  shadow.closePath()
  shadow.fill({ color: 0x000000, alpha: 0.18 })
  parent.addChild(shadow)

  // Camera-space depth helper for face centroids. Smaller zCamForward =
  // farther from camera, so we sort ASCENDING and draw farthest first.
  const yawCos = Math.cos(camera.yaw)
  const yawSin = Math.sin(camera.yaw)
  const pSin = Math.sin(camera.pitch)
  const pCos = Math.cos(camera.pitch)
  const cameraDepth = (cx: number, cy: number, cz: number): number => {
    const yRot = cx * yawSin + cy * yawCos
    return yRot * pSin + cz * pCos
  }

  const mx = x + w / 2
  const my = y + d / 2
  const mz = z + h / 2

  type Face = { corners: Vec2[]; depth: number; shadeMul: number }
  const faces: Face[] = [
    { corners: [tbl, tbr, tfr, tfl], depth: cameraDepth(mx, my, z + h), shadeMul: 1.0 },
    { corners: [bbl, bfl, bfr, bbr], depth: cameraDepth(mx, my, z), shadeMul: 0.35 },
    { corners: [bfl, bfr, tfr, tfl], depth: cameraDepth(mx, y + d, mz), shadeMul: 0.7 },
    { corners: [bbr, bbl, tbl, tbr], depth: cameraDepth(mx, y, mz), shadeMul: 0.55 },
    { corners: [bbl, bfl, tfl, tbl], depth: cameraDepth(x, my, mz), shadeMul: 0.5 },
    { corners: [bbr, bfr, tfr, tbr], depth: cameraDepth(x + w, my, mz), shadeMul: 0.65 },
  ]
  faces.sort((a, b) => a.depth - b.depth)

  for (const face of faces) {
    const faceColor = shadeColor(color, face.shadeMul)
    const g = new Graphics()
    g.moveTo(face.corners[0].x, face.corners[0].y)
    for (let i = 1; i < 4; i++) g.lineTo(face.corners[i].x, face.corners[i].y)
    g.closePath()
    g.fill({ color: faceColor, alpha: 0.95 })
    g.stroke({ width: 1, color: 0x000000, alpha: 0.25 })
    parent.addChild(g)
  }

  if (label) {
    const labelPos = project({ x: x + w / 2, y: y + d + 30, z: 0 })
    const t = new Text({
      text: label,
      style: {
        fontFamily: 'SF Mono, Fira Code, monospace',
        fontSize: 11,
        fontWeight: '500',
        fill: 0x6b6560,
      },
    })
    t.anchor.set(0.5, 0)
    t.x = labelPos.x
    t.y = labelPos.y
    parent.addChild(t)
  }
}

function shadeColor(color: number, mul: number): number {
  // Component-wise multiply for fake directional lighting. Clamps each
  // channel at 255 so highlight sides don't wash to white if shadeMul
  // ever exceeds 1.
  const r = Math.min(255, Math.floor(((color >> 16) & 0xff) * mul))
  const g = Math.min(255, Math.floor(((color >> 8) & 0xff) * mul))
  const b = Math.min(255, Math.floor((color & 0xff) * mul))
  return (r << 16) | (g << 8) | b
}
