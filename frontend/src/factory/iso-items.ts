// ItemSimulator — drives factory items along the path graph.
//
// State per item is tiny: which segment it's on, how far along, how
// fast, and the meshes representing it. Per frame, the simulator
// advances `progress` by `speed * dt`; on overflow it transfers the
// item to the next segment in the chain (subtracting the previous
// segment's length so any leftover travel applies to the new one).
// A dead-end (segment.next is empty) disposes the item's meshes.
//
// Spawners are registered as (segment, interval, accumulator) and
// fire on the same tick — a `while` loop catches the case where
// dt > interval (rare, but a paused tab could otherwise miss
// spawns).
//
// Rendering is one shell + core mesh per item, matching the queued-
// chip language so on-belt items read as the same kind of token
// that's stacked on the station's pad. We'll instance these with
// thinInstances when the item count grows; for now each item is
// two CreateCylinder calls — fine for the dozen-item demo scale.

import {
  Color3,
  type Mesh,
  MeshBuilder,
  PBRMaterial,
  type Scene,
  TransformNode,
} from '@babylonjs/core'

import { BELT_WORLD_SPEED } from './iso-belt'
import type { PathSegment } from './iso-path'

const ITEM_DIAM = 32
const ITEM_HEIGHT = 14
// Small clearance so the item visibly hovers above the belt rather
// than z-fighting against the chevron texture's top surface.
const ITEM_LIFT = 0.5
const ITEM_CORE_DIAM_FRAC = 0.5
const ITEM_CORE_HEIGHT_FRAC = 0.6

// Items default to riding at the belt's surface speed, so a chip and
// the chevrons under it move at the same rate. Override per-spawn if
// we ever introduce slow conveyors or sprint sections.
const DEFAULT_SPEED = BELT_WORLD_SPEED

// Per-namespace core glow tuning. Status is conveyed by which station
// an item is moving to/from (the factory layout IS the status), so
// the chip's color channel is free to encode something stable per
// entity — its namespace (GitHub repo or Jira project). Same hue
// every time the user sees a chip from `triage-factory` or `SKY`,
// no config UI required.
const CORE_HUE_SATURATION = 0.78
const CORE_HUE_VALUE = 1.0
const CORE_EMISSIVE_INTENSITY = 1.4

/** Deterministic string → hue ([0, 360)). djb2-derived; the exact
 *  bit-mixing isn't important, just that the same input always gives
 *  the same output and small input changes give large hue jumps. */
function hashHue(s: string): number {
  let h = 5381
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) + h + s.charCodeAt(i)) >>> 0
  }
  return h % 360
}

interface FactoryItem {
  id: string
  segment: PathSegment
  /** Distance traveled along current segment, in world units. */
  progress: number
  /** Forward speed in world-units per second. */
  speed: number
  /** Parent transform — owns position and heading. The shell + core
   *  are children with a fixed `rotation.x = π/2` to stand them up
   *  in the parent's local frame. This split is needed because
   *  Babylon's `rotation` property applies as Ry*Rx*Rz, which would
   *  apply heading-around-Z BEFORE stand-up-around-X if both lived
   *  on the same node — at non-trivial headings the cylinder ends
   *  up horizontal instead of upright. With the parent owning Z and
   *  the child owning X, each rotation operates in the right local
   *  frame and the cylinder always stands upright. */
  root: TransformNode
  shell: Mesh
  core: Mesh
}

interface Spawner {
  segment: PathSegment
  interval: number
  /** Time since last spawn (or since registration). When this exceeds
   *  `interval` we spawn one item and subtract `interval`. */
  accumulator: number
  speed: number
  /** If set, the spawner round-robins through these on each spawn so
   *  the demo scene shows a mix of repo/project colors. Real spawning
   *  will pass per-entity namespaces, not cycle through a list. */
  namespaces?: string[]
  namespaceCursor: number
}

export interface SpawnerOptions {
  speed?: number
  /** Round-robin through these namespaces — one chip per spawn, then
   *  advance the cursor. Mostly for demo purposes; production spawns
   *  will use spawnItem directly with the entity's namespace. */
  namespaces?: string[]
}

export interface SpawnOptions {
  speed?: number
  /** Repo (GH) or project (Jira) the entity belongs to. Hashed to a
   *  hue for the chip's core; same name → same hue across sessions. */
  namespace?: string
}

export class ItemSimulator {
  private items: FactoryItem[] = []
  private spawners: Spawner[] = []
  private nextId = 0
  private observer: ReturnType<Scene['onBeforeRenderObservable']['add']>
  private scene: Scene
  private shellMat: PBRMaterial
  private coreMat: PBRMaterial
  /** One core material per namespace, lazily created and reused. The
   *  shell stays shared across all items (glass-token language is
   *  uniform); only the core hue varies. */
  private coreMaterials: Map<string, PBRMaterial> = new Map()

  constructor(scene: Scene, shellMat: PBRMaterial, coreMat: PBRMaterial) {
    this.scene = scene
    this.shellMat = shellMat
    this.coreMat = coreMat
    this.observer = scene.onBeforeRenderObservable.add(() => this.tick())
  }

  /** Register a spawner that emits one item every `intervalSeconds`
   *  at the given segment's start. Multiple spawners can share a
   *  segment (no de-dup); items just stack up if the spawn rate
   *  exceeds the segment's throughput. */
  startSpawner(segment: PathSegment, intervalSeconds: number, options: SpawnerOptions = {}): void {
    this.spawners.push({
      segment,
      interval: intervalSeconds,
      accumulator: 0,
      speed: options.speed ?? DEFAULT_SPEED,
      namespaces: options.namespaces,
      namespaceCursor: 0,
    })
  }

  /** Spawn a single item at progress=0 of the given segment.
   *  Public so callers can manually trigger spawns (e.g., for
   *  per-entity emission once the data layer is wired). */
  spawnItem(segment: PathSegment, options: SpawnOptions = {}): void {
    const id = `item-${this.nextId++}`
    const root = new TransformNode(`${id}-root`, this.scene)
    const shell = MeshBuilder.CreateCylinder(
      `${id}-shell`,
      { diameter: ITEM_DIAM, height: ITEM_HEIGHT, tessellation: 28 },
      this.scene,
    )
    shell.rotation.x = Math.PI / 2
    shell.material = this.shellMat
    shell.parent = root
    const core = MeshBuilder.CreateCylinder(
      `${id}-core`,
      {
        diameter: ITEM_DIAM * ITEM_CORE_DIAM_FRAC,
        height: ITEM_HEIGHT * ITEM_CORE_HEIGHT_FRAC,
        tessellation: 20,
      },
      this.scene,
    )
    core.rotation.x = Math.PI / 2
    core.material = this.getCoreMaterial(options.namespace)
    core.parent = root
    const item: FactoryItem = {
      id,
      segment,
      progress: 0,
      speed: options.speed ?? DEFAULT_SPEED,
      root,
      shell,
      core,
    }
    this.items.push(item)
    this.updatePose(item)
  }

  private getCoreMaterial(namespace: string | undefined): PBRMaterial {
    if (!namespace) return this.coreMat
    const cached = this.coreMaterials.get(namespace)
    if (cached) return cached
    const hue = hashHue(namespace)
    const m = new PBRMaterial(`item-core-${namespace}`, this.scene)
    m.albedoColor = Color3.Black()
    m.emissiveColor = Color3.FromHSV(hue, CORE_HUE_SATURATION, CORE_HUE_VALUE)
    m.emissiveIntensity = CORE_EMISSIVE_INTENSITY
    m.metallic = 0
    m.roughness = 1
    this.coreMaterials.set(namespace, m)
    return m
  }

  private tick(): void {
    const dt = this.scene.getEngine().getDeltaTime() / 1000

    // Advance spawners. The `while` loop catches catch-up scenarios
    // (e.g., a backgrounded tab returning with dt > interval).
    for (const sp of this.spawners) {
      sp.accumulator += dt
      while (sp.accumulator >= sp.interval) {
        sp.accumulator -= sp.interval
        let namespace: string | undefined
        if (sp.namespaces && sp.namespaces.length > 0) {
          namespace = sp.namespaces[sp.namespaceCursor]
          sp.namespaceCursor = (sp.namespaceCursor + 1) % sp.namespaces.length
        }
        this.spawnItem(sp.segment, { speed: sp.speed, namespace })
      }
    }

    // Advance items. Items that hit a dead end have their meshes
    // disposed and are filtered out of the live list.
    const survivors: FactoryItem[] = []
    for (const item of this.items) {
      item.progress += item.speed * dt
      let alive = true
      while (item.progress > item.segment.length) {
        if (item.segment.next.length === 0) {
          // Dead end — item is consumed (disappears at recess back,
          // pole terminus, etc.). Future: hand off to station/router
          // processing instead of disposing.
          this.disposeItem(item)
          alive = false
          break
        }
        item.progress -= item.segment.length
        item.segment = item.segment.next[0]
      }
      if (alive) {
        this.updatePose(item)
        survivors.push(item)
      }
    }
    this.items = survivors
  }

  private updatePose(item: FactoryItem): void {
    const { position, tangent } = item.segment.sample(item.progress)
    // Path z = belt-top surface. Lift by half the item's height plus
    // ITEM_LIFT so the item's visual base sits just above the belt.
    const z = position.z + ITEM_HEIGHT / 2 + ITEM_LIFT
    item.root.position.set(position.x, position.y, z)
    // Heading around world Z, applied to the parent. Children handle
    // the stand-up-around-X separately, so the order is correct.
    // Symmetric cylinders won't show this rotation, but it's the
    // right hook for asymmetric items (label, arrow) later.
    item.root.rotation.z = Math.atan2(tangent.y, tangent.x)
  }

  private disposeItem(item: FactoryItem): void {
    item.shell.dispose()
    item.core.dispose()
    item.root.dispose()
  }

  destroy(): void {
    if (this.observer) {
      this.scene.onBeforeRenderObservable.remove(this.observer)
    }
    for (const item of this.items) {
      this.disposeItem(item)
    }
    for (const m of this.coreMaterials.values()) {
      m.dispose()
    }
    this.coreMaterials.clear()
    this.items = []
    this.spawners = []
  }
}
