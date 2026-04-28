// Camera-driven 3D projection for the factory view.
//
// Two projection modes:
//
//   ORTHOGRAPHIC (default) — parallel projection, no vanishing point.
//     Sizes stay constant across depth. The standard for factory /
//     strategy game layouts because the floor stays readable at any
//     orientation.
//
//   PERSPECTIVE — divide-by-depth, things farther from camera shrink.
//     Same camera angle controls (pitch/yaw); the camera also has a
//     finite distance and focal length that determine how strong the
//     foreshortening is. Matches the default Unity / After Effects
//     camera behavior.
//
// Camera state:
//
//   pitch — rotation around world x-axis. 0 = top-down. Larger pitch
//           tilts the camera forward, exposing top + front faces and
//           compressing the floor along screen y.
//   yaw   — rotation around world z-axis. 0 = looking due north (no
//           horizontal swivel). Positive yaw turns the camera to expose
//           the right faces of upright objects.
//   zoom  — uniform scale applied after projection.
//   panX/panY — screen-space offset applied after zoom.
//   mode  — 'orthographic' | 'perspective'.
//   cameraDistance — perspective only; how far from the focal point
//           (world origin) the camera sits. Larger = weaker perspective.
//   focalLength — perspective only; the multiplier applied to camera-
//           space x/y before the depth divide. With focalLength ==
//           cameraDistance, points at the focal plane (world origin)
//           render at the same screen size as orthographic.
//
// World convention (unchanged across modes):
//
//   x — east-west; +x = "right" in world.
//   y — north-south DEPTH. +y = "closer to the viewer."
//   z — UP. +z lifts a point upward on screen.

export type ProjectionMode = 'orthographic' | 'perspective'

export interface Vec3 {
  x: number
  y: number
  z: number
}

export interface Vec2 {
  x: number
  y: number
}

export interface CameraState {
  pitch: number
  yaw: number
  zoom: number
  panX: number
  panY: number
  mode: ProjectionMode
  cameraDistance: number
  focalLength: number
  /** Anchor for perspective foreshortening. The focal plane (where
   * scale=1, i.e., points project at the same screen size as
   * orthographic) passes through this world point perpendicular to the
   * camera's forward axis. Caller is responsible for keeping this in
   * sync with what the user is "looking at" (typically the world point
   * under the screen center) — without that, perspective foreshortens
   * around world origin, which feels like the scene shrinks toward
   * the wrong vanishing point. */
  focalPoint: Vec3
}

const MAX_PITCH = Math.PI / 2 - 0.01
const MIN_ZOOM = 0.2
const MAX_ZOOM = 5

// Default perspective parameters. cameraDistance is set well outside the
// debug floor size (1200 world units) so the floor doesn't approach or
// exceed the camera plane during normal viewing. focalLength ==
// cameraDistance means the focal plane (world origin) renders at the
// same scale in both modes — switching between modes only affects how
// off-origin points are scaled.
const DEFAULT_CAMERA_DISTANCE = 3000
const DEFAULT_FOCAL_LENGTH = 3000

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v
}

function wrapAngle(a: number): number {
  const twoPi = Math.PI * 2
  return ((a % twoPi) + twoPi) % twoPi
}

export class Camera {
  private state: CameraState = {
    pitch: 0,
    yaw: 0,
    zoom: 1,
    panX: 0,
    panY: 0,
    mode: 'orthographic',
    cameraDistance: DEFAULT_CAMERA_DISTANCE,
    focalLength: DEFAULT_FOCAL_LENGTH,
    focalPoint: { x: 0, y: 0, z: 0 },
  }
  private listeners = new Set<() => void>()

  get pitch(): number {
    return this.state.pitch
  }
  get yaw(): number {
    return this.state.yaw
  }
  get zoom(): number {
    return this.state.zoom
  }
  get panX(): number {
    return this.state.panX
  }
  get panY(): number {
    return this.state.panY
  }
  get mode(): ProjectionMode {
    return this.state.mode
  }
  get focalPoint(): Vec3 {
    return { ...this.state.focalPoint }
  }

  setPitch(rad: number): void {
    this.state.pitch = clamp(rad, 0, MAX_PITCH)
    this.notify()
  }
  setYaw(rad: number): void {
    this.state.yaw = wrapAngle(rad)
    this.notify()
  }
  setZoom(z: number): void {
    this.state.zoom = clamp(z, MIN_ZOOM, MAX_ZOOM)
    this.notify()
  }
  setPan(x: number, y: number): void {
    this.state.panX = x
    this.state.panY = y
    this.notify()
  }
  pan(dx: number, dy: number): void {
    this.state.panX += dx
    this.state.panY += dy
    this.notify()
  }
  setMode(mode: ProjectionMode): void {
    if (this.state.mode === mode) return
    this.state.mode = mode
    this.notify()
  }
  toggleMode(): void {
    this.setMode(this.state.mode === 'orthographic' ? 'perspective' : 'orthographic')
  }

  setFocalPoint(p: Vec3): void {
    this.state.focalPoint = { x: p.x, y: p.y, z: p.z }
    this.notify()
  }

  /** Orthographic-only inverse: where on the floor (z=0) does the screen
   * point land if we ignore perspective? Used by callers that need to
   * compute a stable focal point from screen coords without recursion
   * (perspective math depends on the focal point, so the focal point
   * itself can't be computed via the perspective inverse). */
  orthographicScreenToFloor(s: Vec2): Vec3 {
    const sx = (s.x - this.state.panX) / this.state.zoom
    const sy = (s.y - this.state.panY) / this.state.zoom
    const cosY = Math.cos(this.state.yaw)
    const sinY = Math.sin(this.state.yaw)
    const cosP = Math.cos(this.state.pitch)
    const yRot = sy / cosP
    return {
      x: sx * cosY + yRot * sinY,
      y: -sx * sinY + yRot * cosY,
      z: 0,
    }
  }

  reset(panX: number = 0, panY: number = 0): void {
    this.state.pitch = 0
    this.state.yaw = 0
    this.state.zoom = 1
    this.state.panX = panX
    this.state.panY = panY
    // Reset preserves the projection mode — toggling perspective is a
    // separate user concern from reorienting the view.
    this.notify()
  }

  /** Project a world point to screen space using the current camera
   * state. Returns NaN coordinates if the point is behind the camera in
   * perspective mode (caller should skip rendering). */
  worldToScreen(p: Vec3): Vec2 {
    const cosY = Math.cos(this.state.yaw)
    const sinY = Math.sin(this.state.yaw)
    const cosP = Math.cos(this.state.pitch)
    const sinP = Math.sin(this.state.pitch)

    // Yaw rotation (around world z) into camera-aligned x/y.
    const xRot = p.x * cosY - p.y * sinY
    const yRot = p.x * sinY + p.y * cosY

    // Pitch rotation: yRot/z mix into camera "screen y" (yCam) and
    // "depth" axis (zCamForward = displacement along the camera's
    // look-at direction, with origin at the focal plane).
    const xCam = xRot
    const yCam = yRot * cosP - p.z * sinP
    const zCamForward = yRot * sinP + p.z * cosP

    let projX = xCam
    let projY = yCam

    if (this.state.mode === 'perspective') {
      // Foreshortening is anchored to the focal point: the focal plane
      // (perpendicular to camera forward, passing through focalPoint)
      // sits at depth=cameraDistance from the camera, so points ON the
      // focal plane render at the same scale as orthographic. Points
      // closer than the focal plane appear larger; farther appears
      // smaller. Without this, perspective foreshortens around world
      // origin and the scene seems to shrink toward the wrong point.
      const f = this.state.focalPoint
      const focalYRot = f.x * sinY + f.y * cosY
      const focalZCamForward = focalYRot * sinP + f.z * cosP
      const depth = this.state.cameraDistance + focalZCamForward - zCamForward
      if (depth <= 0) return { x: NaN, y: NaN }
      const scale = this.state.focalLength / depth
      projX = xCam * scale
      projY = yCam * scale
    }

    return {
      x: projX * this.state.zoom + this.state.panX,
      y: projY * this.state.zoom + this.state.panY,
    }
  }

  /** Inverse projection: given a screen point, return the world point on
   * the floor (z=0) under it. Used for hit-testing and pivot-locking
   * during rotate/zoom. Both projection modes are handled exactly. */
  screenToFloor(s: Vec2): Vec3 {
    // Undo pan + zoom first.
    const sx = (s.x - this.state.panX) / this.state.zoom
    const sy = (s.y - this.state.panY) / this.state.zoom

    const cosY = Math.cos(this.state.yaw)
    const sinY = Math.sin(this.state.yaw)
    const cosP = Math.cos(this.state.pitch)
    const sinP = Math.sin(this.state.pitch)

    let xCam: number
    let yRot: number

    if (this.state.mode === 'perspective') {
      // Inverse of the focal-anchored perspective projection. With
      // Q = cameraDistance + focalZCamForward, the forward formula is:
      //   sx = xCam * f / (Q - yRot * sinP)
      //   sy = yRot * cosP * f / (Q - yRot * sinP)
      // Solving for yRot then back-solving xCam:
      //   yRot = sy * Q / (f * cosP + sy * sinP)
      const D = this.state.cameraDistance
      const f = this.state.focalLength
      const focal = this.state.focalPoint
      const focalYRot = focal.x * sinY + focal.y * cosY
      const focalZCamForward = focalYRot * sinP + focal.z * cosP
      const Q = D + focalZCamForward
      const denom = f * cosP + sy * sinP
      if (Math.abs(denom) < 1e-9) {
        // Camera ray is parallel to the floor — no intersection.
        return { x: 0, y: 0, z: 0 }
      }
      yRot = (sy * Q) / denom
      const depth = Q - yRot * sinP
      const invScale = depth / f
      xCam = sx * invScale
    } else {
      // Orthographic: yRot * cosP = sy → yRot = sy / cosP. xCam = sx.
      yRot = sy / cosP
      xCam = sx
    }

    // Undo yaw — the 2x2 rotation matrix is orthonormal so its inverse
    // is its transpose.
    return {
      x: xCam * cosY + yRot * sinY,
      y: -xCam * sinY + yRot * cosY,
      z: 0,
    }
  }

  onChange(cb: () => void): () => void {
    this.listeners.add(cb)
    return () => {
      this.listeners.delete(cb)
    }
  }

  private notify(): void {
    for (const cb of this.listeners) cb()
  }
}
