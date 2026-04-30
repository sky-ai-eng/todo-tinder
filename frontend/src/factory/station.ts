// Station renderer — one machine on the factory floor, populated from event
// catalog metadata + the live predicate schema fetched from the API.
//
// Anatomy:
//
//   ┌───────────────────────────────────────────┐
//   │ ●  PR Opened                 [ github ]   │  header band
//   │ ┌───────────────────────────────────────┐ │
//   │ │ ┏━┓        ✦  (watermark)         ┏━┓ │ │  core chamber: glyph
//   │●│  │       ▢▢▢▢▢▢                    │●│   + entity pile inside
//   │ │ ┗━┛       ▢▢▢▢▢▢                ┗━┛ │ │  port nubs on left/right edges
//   │ └───────────────────────────────────────┘ │
//   │                                           │  reserved (rhythm strip TBD)
//   └───────────────────────────────────────────┘
//
// The body is a raised platform: stronger drop-shadow stack + dark bottom
// rim so it reads as elevated above the conveyor floor. The chamber is
// carved INTO the body — dark outer lip, top-edge inset shadow, bottom-
// edge highlight — and is where parked entities pile as small luminous
// chips. Port nubs on the left/right edges are the visible docking
// points the belts snap to.
//
// Presentation data (label, category, lifecycle, glyph) comes from
// factory/events.ts.

import { Container, Graphics, Text } from 'pixi.js'
import type { FieldSchema } from '../types'
import type { FactoryEvent } from './events'
import { drawGlyph } from './glyphs'

const W = 260
const H = 180
const R = 18
const CORE_R = 12
const HEADER_H = 40
// Bottom strip reserved for future activity-rhythm content (sparkline +
// last-arrival timestamp). Predicate chips and entity pills used to live
// here — both replaced: chips were schema metadata that didn't describe
// what's actually happening at the station, and the pill list got fuzzy
// and truncated past 3 entries. Kept as empty space for now so the chamber
// has visual weight on top and the bottom isn't a hard rim.
const CHIPS_H = 24
const CORE_PAD_X = 14
const CORE_PAD_TOP = 4
const CORE_PAD_BOT = 6

/** Conveyor belt width — exported so scene.ts can draw the belt flush with
 * the port stubs. */
export const BELT_WIDTH = 28

/** How far each port stub protrudes outward from the station frame edge.
 * Belts connect at the stub's outer end, not the station edge, so the
 * conveyor material is visually continuous. */
export const PORT_STUB_LEN = 24

// Port offsets in station-local coords. Port y is forced to 0 (the
// station's vertical center, which sits on the grid row line) so belts
// between stations and other nodes — whose ports are all at center.y —
// are perfectly horizontal. The core chamber's midline is ~3px below
// this axis; that offset is small enough to read as intentional
// asymmetry between header and chip strip rather than a bent belt.
const CORE_Y = -H / 2 + HEADER_H + CORE_PAD_TOP
const CORE_H = H - HEADER_H - CHIPS_H - CORE_PAD_TOP - CORE_PAD_BOT
const PORT_LOCAL_Y = 0

const CATEGORY_COLOR: Record<string, number> = {
  pr_flow: 0xc47a5a,
  pr_review: 0x7a9aad,
  pr_ci: 0x6ea87a,
  pr_signals: 0x9a7aad,
  jira_flow: 0xb8943a,
  jira_signals: 0x8a8480,
}

const TEXT_PRIMARY = 0x1a1a1a
const TEXT_TERTIARY = 0xa09a94
const STATE_ENABLED = 0x5a8c6a
const RIM_HIGHLIGHT = 0xffffff

export interface StationOptions {
  event: FactoryEvent
  /** Predicate-field schema for this event type. Currently unused inside
   * the station — the abstract schema metadata didn't tell viewers what
   * was happening at the station, so the predicate-chip strip was
   * removed in favor of the entity pile inside the chamber. Kept in the
   * options to preserve the call-site shape; if predicate visibility
   * comes back (e.g., in a near-zoom info panel) it'll be re-consumed
   * here. */
  fields: FieldSchema[]
  /** Whether any prompt is currently wired to this event. Dims the station when false. */
  enabled?: boolean
  center: { x: number; y: number }
}

/** A belt dock point. `dir` is the outward unit vector the port faces —
 * belts exit along this direction and arrive against it, which lets the
 * belt renderer build smooth S-curves that tangent-match the port on both
 * ends. Left ports face west (-1, 0); right ports face east (1, 0). */
export interface Port {
  x: number
  y: number
  dir: { x: number; y: number }
}

export interface StationHandle {
  kind: 'station'
  container: Container
  center: { x: number; y: number }
  eventType: string
  leftPort: Port
  rightPort: Port
  /** Stations don't expose top/bottom ports — those live on splitter/merger
   * nodes only. Declared for shape-compatibility with the GraphNode union
   * consumers use in the routing layer. */
  topPort?: undefined
  bottomPort?: undefined
  /** Station world-space size — used by the detail overlay to compute a
   * screen-space anchor rect without re-deriving from hard-coded constants. */
  worldSize: { w: number; h: number; coreY: number; coreH: number }
  /** dt is seconds since last frame; scale is the current viewport scale
   * (1 = neutral, <1 = zoomed out, >1 = zoomed in). The station toggles
   * LOD sub-groups based on scale — at near zoom the predicate chips and
   * glyph hide so an HTML overlay can take over the interior. */
  update(dt: number, scale: number): void
  /** Set the count of entities currently parked at this station. At far
   * zoom a small badge renders this near the glyph in place of the
   * individual item pills, which get too dense to read when the whole
   * factory fits on screen. Zero hides the badge. */
  setItemCount(n: number): void
  /** Set the list of entities currently parked at this station. At mid
   * zoom, the predicate chip row is replaced by these pills so the
   * station self-describes "these PRs are waiting here" rather than
   * showing static schema hints. Empty list falls back to predicate
   * chips. At near/far zoom the entity pills hide (overlay / badge
   * take over). */
  setEntities(entities: Array<{ label: string; mine: boolean }>): void
}

/** Viewport scale at or above which the station enters "near" LOD: chips
 * and glyph hide so the HTML detail overlay can render active runs and
 * throughput in the freed space. */
export const NEAR_ZOOM_THRESHOLD = 1.5

/** Viewport scale below which the station enters "far" LOD: the dense
 * header + core + chips visuals all collapse to a single oversized label
 * so the station stays legible when the whole factory fits on screen.
 * 14px Pixi text at scale 0.5 renders at ~7 CSS px — unreadable — so we
 * swap in a 36px label that holds up when the viewport is zoomed out. */
export const FAR_ZOOM_THRESHOLD = 0.6

export function buildStation(parent: Container, opts: StationOptions): StationHandle {
  const { event, enabled = true, center } = opts
  const color = event.tint ?? CATEGORY_COLOR[event.category] ?? CATEGORY_COLOR.pr_flow

  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  const fx = -W / 2
  const fy = -H / 2

  // Drop shadow — three stacked layers fake a soft falloff without paying
  // for a blur filter. Outer halo (large, barely visible) + middle band
  // (medium, soft) + tight contact shadow (close to the body, darker).
  // Together they read as "raised platform on the factory floor" rather
  // than "card with a single weak shadow." The contact shadow is offset
  // mostly DOWN with a tiny x-bias — mimics overhead-and-slightly-front
  // lighting consistent with the top sheen direction.
  const shadowFar = new Graphics()
  shadowFar.roundRect(fx - 14, fy + 16, W + 28, H + 22, R + 12)
  shadowFar.fill({ color: 0x000000, alpha: 0.04 })
  root.addChild(shadowFar)

  const shadowMid = new Graphics()
  shadowMid.roundRect(fx - 6, fy + 10, W + 12, H + 14, R + 6)
  shadowMid.fill({ color: 0x000000, alpha: 0.07 })
  root.addChild(shadowMid)

  const shadowNear = new Graphics()
  shadowNear.roundRect(fx - 1, fy + 4, W + 2, H + 6, R + 1)
  shadowNear.fill({ color: 0x000000, alpha: 0.1 })
  root.addChild(shadowNear)

  // Port stubs — short belt extensions protruding from the left and right
  // edges of the frame. Drawn BEFORE the frame body so the body overlaps
  // the stub's inner end, making the stub appear to emerge from inside the
  // station. Belts attach at the stub's outer end.
  drawPortStub(root, fx, PORT_LOCAL_Y, -1, color)
  drawPortStub(root, fx + W, PORT_LOCAL_Y, 1, color)

  // Frame body — bumped to higher opacity so the station reads as a solid
  // raised platform rather than a glass plate the conveyors show through.
  // The whole point of the depth redesign is "stations protrude, conveyors
  // sit on the background plane" — translucent body undermined that.
  const body = new Graphics()
  body.roundRect(fx, fy, W, H, R)
  body.fill({ color: 0xffffff, alpha: 0.96 })
  root.addChild(body)

  // Category wash on the frame — barely-there warmth hinting at the row.
  const tint = new Graphics()
  tint.roundRect(fx, fy, W, H, R)
  tint.fill({ color, alpha: 0.05 })
  root.addChild(tint)

  // Top sheen — the "light catches the top" liquid-glass cue, slightly
  // stronger now since the body is more opaque.
  const sheen = new Graphics()
  sheen.roundRect(fx + 4, fy + 4, W - 8, H / 3, R - 6)
  sheen.fill({ color: 0xffffff, alpha: 0.42 })
  root.addChild(sheen)

  // Outer hairline + inner rim highlight + bottom edge darkening.
  const outerRim = new Graphics()
  outerRim.roundRect(fx, fy, W, H, R)
  outerRim.stroke({ width: 1, color: 0x000000, alpha: 0.12, alignment: 1 })
  root.addChild(outerRim)

  const innerRim = new Graphics()
  innerRim.roundRect(fx + 1, fy + 1, W - 2, H - 2, R - 1)
  innerRim.stroke({ width: 1, color: RIM_HIGHLIGHT, alpha: 0.95, alignment: 0 })
  root.addChild(innerRim)

  // Bottom-edge contact line — short dark stroke sitting just inside the
  // bottom rim. Sells "the platform meets the floor here, this side is
  // away from the light." Subtle but a big contributor to the raised
  // feel; without it, the body floats ambiguously.
  const bottomShade = new Graphics()
  bottomShade.moveTo(fx + R, fy + H - 1.5)
  bottomShade.lineTo(fx + W - R, fy + H - 1.5)
  bottomShade.stroke({ width: 1.25, color: 0x000000, alpha: 0.16 })
  root.addChild(bottomShade)

  // ── Detail layer ──────────────────────────────────────────────────────────
  // Wraps everything that belongs to the "mid+near" LOD: header visuals,
  // core chamber, HUD brackets. At far zoom this whole layer hides and the
  // farLayer below (a single big label) takes over — 14px Pixi text shrinks
  // to unreadable at scale 0.4, so we need a distinct simplified view.
  const detailLayer = new Container()
  root.addChild(detailLayer)

  // ── Header ────────────────────────────────────────────────────────────────
  const headerBottomY = fy + HEADER_H
  const headerDivider = new Graphics()
  headerDivider.moveTo(fx + 14, headerBottomY)
  headerDivider.lineTo(fx + W - 14, headerBottomY)
  headerDivider.stroke({ width: 1, color: 0x000000, alpha: 0.05 })
  detailLayer.addChild(headerDivider)

  const pip = new Graphics()
  pip.circle(fx + 16, fy + 20, 4)
  pip.fill({ color: enabled ? STATE_ENABLED : TEXT_TERTIARY, alpha: enabled ? 0.9 : 0.5 })
  detailLayer.addChild(pip)

  const title = new Text({
    text: event.label,
    // resolution: 3 lets the raster stay sharp up to 3× zoom (NEAR). Pixi
    // rasters Text once at creation-time DPI and then transforms the
    // texture at draw — without this bump, text gets fuzzy at near zoom.
    resolution: 3,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 14,
      fontWeight: '600',
      fill: TEXT_PRIMARY,
      letterSpacing: 0.1,
    },
  })
  title.anchor.set(0, 0.5)
  title.x = fx + 28
  title.y = fy + 20
  detailLayer.addChild(title)

  // Source badge — rounded pill, right side of header.
  const badgeText = new Text({
    text: event.source,
    resolution: 3,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 9,
      fontWeight: '700',
      fill: color,
      letterSpacing: 0.8,
    },
  })
  const badgeW = Math.ceil(badgeText.width) + 14
  const badgeH = 16
  const badgeX = fx + W - 14 - badgeW
  const badgeY = fy + 12
  const badge = new Graphics()
  badge.roundRect(badgeX, badgeY, badgeW, badgeH, badgeH / 2)
  badge.fill({ color, alpha: 0.1 })
  badge.stroke({ width: 0.75, color, alpha: 0.35 })
  detailLayer.addChild(badge)
  badgeText.anchor.set(0.5, 0.5)
  badgeText.x = badgeX + badgeW / 2
  badgeText.y = badgeY + badgeH / 2 + 0.5
  detailLayer.addChild(badgeText)

  // ── Core chamber ──────────────────────────────────────────────────────────
  // The chamber is a real recess carved INTO the station body — entities
  // pile inside it. Three layered cues sell the depth:
  //   1. Dark rim — chamber outline with stronger fill, gives the carved
  //      edge a visible thickness.
  //   2. Top inner shadow — a dark band along the top inside edge that
  //      reads as "shadow falls into the recessed area."
  //   3. Bottom highlight — bright thin line on the bottom inner edge
  //      where the floor catches what little light reaches in.
  // The interior fill is darker and slightly cooler than the body so the
  // chamber reads as "interior shadow," distinct from the well-lit body.
  const coreX = fx + CORE_PAD_X
  const coreW = W - CORE_PAD_X * 2

  // Carved rim — slightly larger filled shape behind the floor. The 2px
  // overhang creates a visible dark "lip" around the chamber, the chief
  // depth cue.
  const coreRim = new Graphics()
  coreRim.roundRect(coreX - 2, CORE_Y - 2, coreW + 4, CORE_H + 4, CORE_R + 2)
  coreRim.fill({ color: 0x000000, alpha: 0.18 })
  detailLayer.addChild(coreRim)

  // Chamber floor — darker and slightly cooler than the warm body. This
  // is the "inside shadow" surface entities sit on.
  const coreFloor = new Graphics()
  coreFloor.roundRect(coreX, CORE_Y, coreW, CORE_H, CORE_R)
  coreFloor.fill({ color: 0xe6e1d8, alpha: 1 })
  detailLayer.addChild(coreFloor)

  // Category tint wash on the floor — same warmth the body has, dialed
  // down so the chamber stays distinctly cooler/darker than its surround.
  const coreTint = new Graphics()
  coreTint.roundRect(coreX, CORE_Y, coreW, CORE_H, CORE_R)
  coreTint.fill({ color, alpha: 0.05 })
  detailLayer.addChild(coreTint)

  // Top-edge inset shadow — gradient simulated as three increasingly faint
  // dark bands stacked at the top inside edge. Sells "shadow pours into
  // the recess from above" — the single biggest cue selling the depth.
  for (let i = 0; i < 3; i++) {
    const band = new Graphics()
    band.roundRect(coreX + 1, CORE_Y + 0.5 + i * 1.2, coreW - 2, 4, CORE_R - 1)
    band.fill({ color: 0x000000, alpha: 0.14 - i * 0.04 })
    detailLayer.addChild(band)
  }

  // Bottom-edge inner highlight — light catches the floor's far edge.
  const coreFloorLight = new Graphics()
  coreFloorLight.moveTo(coreX + CORE_R, CORE_Y + CORE_H - 1.5)
  coreFloorLight.lineTo(coreX + coreW - CORE_R, CORE_Y + CORE_H - 1.5)
  coreFloorLight.stroke({ width: 1, color: 0xffffff, alpha: 0.55 })
  detailLayer.addChild(coreFloorLight)

  // HUD corner brackets — four L-shapes inset inside the core. Very thin,
  // accent-tinted. Adds "tech object" texture without being busy.
  const BRACKET = 10
  const BM = 8 // margin from core edge
  const bX1 = coreX + BM
  const bY1 = CORE_Y + BM
  const bX2 = coreX + coreW - BM
  const bY2 = CORE_Y + CORE_H - BM
  const brackets = new Graphics()
  brackets.moveTo(bX1, bY1 + BRACKET)
  brackets.lineTo(bX1, bY1)
  brackets.lineTo(bX1 + BRACKET, bY1)
  brackets.moveTo(bX2 - BRACKET, bY1)
  brackets.lineTo(bX2, bY1)
  brackets.lineTo(bX2, bY1 + BRACKET)
  brackets.moveTo(bX1, bY2 - BRACKET)
  brackets.lineTo(bX1, bY2)
  brackets.lineTo(bX1 + BRACKET, bY2)
  brackets.moveTo(bX2 - BRACKET, bY2)
  brackets.lineTo(bX2, bY2)
  brackets.lineTo(bX2, bY2 - BRACKET)
  brackets.stroke({ width: 1, color, alpha: 0.4 })
  detailLayer.addChild(brackets)

  // ── Far layer ─────────────────────────────────────────────────────────────
  // Visible only at far zoom (scale < FAR_ZOOM_THRESHOLD). Three pieces,
  // all in the category color so the station reads as a single semantic
  // unit when the whole factory fits on screen:
  //   - procedural glyph centered above the label (same glyph shown in the
  //     core chamber at mid zoom — gives instant identity: check vs.
  //     merge vs. cross etc.)
  //   - big 32px label below, wraps on multi-line event names
  //   - four HUD-style corner brackets framing the card, echoing the mid-
  //     zoom brackets inside the core chamber
  const farLayer = new Container()
  root.addChild(farLayer)

  const farGlyph = new Container()
  farGlyph.x = 0
  farGlyph.y = -28
  farLayer.addChild(farGlyph)
  drawGlyph(farGlyph, event.glyph, color)
  // drawGlyph emits at its natural procedural size (~32px tall). Scale it
  // up a touch so it reads comfortably against the large label.
  farGlyph.scale.set(1.4)

  const farTitle = new Text({
    text: event.label,
    resolution: 2,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 32,
      fontWeight: '700',
      fill: TEXT_PRIMARY,
      letterSpacing: 0.2,
      align: 'center',
      wordWrap: true,
      wordWrapWidth: W - 40,
    },
  })
  farTitle.anchor.set(0.5, 0.5)
  farTitle.x = 0
  farTitle.y = 28
  farLayer.addChild(farTitle)

  // Halo-style corner brackets on the card itself. Bigger and more inset
  // than the core brackets (10 long / 8 margin) so they read as part of
  // the card silhouette, not competition with the smaller core brackets
  // they replace at this zoom.
  const CARD_BRACKET = 18
  const CARD_BM = 14
  const cx1 = fx + CARD_BM
  const cy1 = fy + CARD_BM
  const cx2 = fx + W - CARD_BM
  const cy2 = fy + H - CARD_BM
  const farBrackets = new Graphics()
  farBrackets.moveTo(cx1, cy1 + CARD_BRACKET)
  farBrackets.lineTo(cx1, cy1)
  farBrackets.lineTo(cx1 + CARD_BRACKET, cy1)
  farBrackets.moveTo(cx2 - CARD_BRACKET, cy1)
  farBrackets.lineTo(cx2, cy1)
  farBrackets.lineTo(cx2, cy1 + CARD_BRACKET)
  farBrackets.moveTo(cx1, cy2 - CARD_BRACKET)
  farBrackets.lineTo(cx1, cy2)
  farBrackets.lineTo(cx1 + CARD_BRACKET, cy2)
  farBrackets.moveTo(cx2 - CARD_BRACKET, cy2)
  farBrackets.lineTo(cx2, cy2)
  farBrackets.lineTo(cx2, cy2 - CARD_BRACKET)
  farBrackets.stroke({ width: 2, color, alpha: 0.6 })
  farLayer.addChild(farBrackets)

  // Count badge rendered next to the far-view glyph. At far zoom
  // individual item pills hide (too dense to read), so the count is the
  // only signal of how many entities are parked here. Positioned to the
  // upper-right of the glyph center so it doesn't occlude the title.
  const farCountBadge = new Container()
  farCountBadge.x = 24
  farCountBadge.y = -38
  farCountBadge.visible = false
  farLayer.addChild(farCountBadge)

  const farCountBg = new Graphics()
  farCountBg.circle(0, 0, 11)
  farCountBg.fill({ color, alpha: 0.9 })
  farCountBg.stroke({ width: 1, color: 0xffffff, alpha: 0.8 })
  farCountBadge.addChild(farCountBg)

  const farCountText = new Text({
    text: '',
    resolution: 2,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 11,
      fontWeight: '700',
      fill: 0xffffff,
    },
  })
  farCountText.anchor.set(0.5, 0.5)
  farCountBadge.addChild(farCountText)

  // Procedural glyph centered in the core. Wrapped in its own container so
  // the near-zoom LOD can hide it, yielding the core's interior to the HTML
  // detail overlay.
  const glyphLayer = new Container()
  glyphLayer.x = coreX + coreW / 2
  glyphLayer.y = CORE_Y + CORE_H / 2
  root.addChild(glyphLayer)
  drawGlyph(glyphLayer, event.glyph, color)

  // ── Entity pile (inside the chamber) ──────────────────────────────────────
  // Parked entities render as small luminous chips inside the chamber
  // recess — the chamber IS the pile, not a bottom strip. Two rows × six
  // columns = 12 visible cap; an overflow "+N" tile takes the last slot
  // when more than 12 are parked. Each chip is mine/other-tinted and
  // placed bottom-up (gravity-friendly stacking), starting from the
  // chamber's bottom-left and filling up. Labels live in the near-zoom
  // HTML overlay; at mid zoom the pile communicates count + ownership
  // mix at a glance, which is the actionable signal.
  const pileLayer = new Container()
  detailLayer.addChild(pileLayer)
  let hasEntities = false

  // Pile sits in the bottom portion of the chamber, leaving the upper
  // portion for the glyph (which fades to a watermark when items are
  // present). Bracket-aware: stays inside the four corner brackets.
  const PILE_COL_W = 26
  const PILE_ROW_H = 14
  const PILE_GAP_X = 4
  const PILE_GAP_Y = 4
  const PILE_COLS = 6
  const PILE_VISIBLE_CAP = 12
  // Keep one row's distance off the chamber floor to avoid kissing the
  // bottom highlight, and one row above the lowest sprite for the second
  // row. Centered horizontally inside the bracket box.
  const pileTotalW = PILE_COLS * PILE_COL_W + (PILE_COLS - 1) * PILE_GAP_X
  const pileStartX = coreX + coreW / 2 - pileTotalW / 2
  // Bottom row baseline sits ~6px above the chamber floor.
  const pileRow0Y = CORE_Y + CORE_H - 6 - PILE_ROW_H
  const pileRow1Y = pileRow0Y - PILE_ROW_H - PILE_GAP_Y

  const rebuildEntityPile = (entities: Array<{ label: string; mine: boolean }>) => {
    pileLayer.removeChildren().forEach((c) => c.destroy({ children: true }))
    hasEntities = entities.length > 0
    if (!hasEntities) return
    const overflow = entities.length > PILE_VISIBLE_CAP
    const slotsForEntities = overflow ? PILE_VISIBLE_CAP - 1 : entities.length
    for (let i = 0; i < slotsForEntities; i++) {
      const ent = entities[i]
      const col = i % PILE_COLS
      const row = Math.floor(i / PILE_COLS)
      const sx = pileStartX + col * (PILE_COL_W + PILE_GAP_X)
      const sy = row === 0 ? pileRow0Y : pileRow1Y
      drawPileChip(pileLayer, sx, sy, PILE_COL_W, PILE_ROW_H, ent.mine)
    }
    if (overflow) {
      const i = PILE_VISIBLE_CAP - 1
      const col = i % PILE_COLS
      const row = Math.floor(i / PILE_COLS)
      const sx = pileStartX + col * (PILE_COL_W + PILE_GAP_X)
      const sy = row === 0 ? pileRow0Y : pileRow1Y
      drawPileOverflow(
        pileLayer,
        sx,
        sy,
        PILE_COL_W,
        PILE_ROW_H,
        entities.length - (PILE_VISIBLE_CAP - 1),
        color,
      )
    }
  }

  // Station-wide dim when disabled.
  if (!enabled) {
    root.alpha = 0.6
  }

  // ── Ambient animation ─────────────────────────────────────────────────────
  // Subtle glyph alpha breathing — reads as "standby, powered on" rather
  // than the static card feeling we had before. No scale pulse (distracting
  // at multi-station scale).
  let t = 0
  const baseAlpha = glyphLayer.alpha
  return {
    kind: 'station',
    container: root,
    center,
    eventType: event.eventType,
    worldSize: { w: W, h: H, coreY: CORE_Y, coreH: CORE_H },
    leftPort: {
      x: center.x - W / 2 - PORT_STUB_LEN,
      y: center.y + PORT_LOCAL_Y,
      dir: { x: -1, y: 0 },
    },
    rightPort: {
      x: center.x + W / 2 + PORT_STUB_LEN,
      y: center.y + PORT_LOCAL_Y,
      dir: { x: 1, y: 0 },
    },
    update(dt: number, scale: number) {
      t += dt
      const breathe = 0.78 + 0.22 * (0.5 + 0.5 * Math.sin(t * 1.5))
      // Glyph fades to a faint watermark when the chamber has entities —
      // the pile is the dominant signal in that state, the glyph is just
      // the station's identity backdrop.
      const glyphTarget = hasEntities ? 0.22 : 1
      glyphLayer.alpha = baseAlpha * breathe * glyphTarget

      // Three LOD tiers, gated on the viewport scale:
      //   far  (scale < 0.6): show only the big label + pip in farLayer;
      //                       hide the dense header / core / glyph / pile
      //   mid  (0.6..1.5):    header + core (with pile inside) + glyph
      //   near (scale >= 1.5): detail stays, but glyph + pile hide so the
      //                       HTML overlay can own the chamber interior
      const far = scale < FAR_ZOOM_THRESHOLD
      const near = scale >= NEAR_ZOOM_THRESHOLD
      const mid = !far && !near
      farLayer.visible = far
      detailLayer.visible = !far
      glyphLayer.visible = mid
      pileLayer.visible = mid
    },
    setItemCount(n: number) {
      if (n <= 0) {
        farCountBadge.visible = false
        return
      }
      farCountText.text = String(n)
      farCountBadge.visible = true
    },
    setEntities(entities: Array<{ label: string; mine: boolean }>) {
      rebuildEntityPile(entities)
    },
  }
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

function drawPortStub(
  parent: Container,
  attachX: number,
  attachY: number,
  direction: 1 | -1,
  color: number,
) {
  // A short flat belt-like stub extending outward from the station frame
  // edge. Same fill and edge treatment as the main belt so the conveyor
  // material reads as one continuous piece. The inner end starts at the
  // station's outer edge (station body overlaps it); the outer end is
  // where the belt docks.
  const outerX = attachX + direction * PORT_STUB_LEN
  const topY = attachY - BELT_WIDTH / 2
  const botY = attachY + BELT_WIDTH / 2
  const leftX = Math.min(attachX, outerX)
  const rightX = Math.max(attachX, outerX)

  // Belt material fill.
  const body = new Graphics()
  body.rect(leftX, topY, rightX - leftX, BELT_WIDTH)
  body.fill({ color: 0xffffff, alpha: 0.82 })
  parent.addChild(body)

  // Warm tint so the stub inherits the category accent.
  const tint = new Graphics()
  tint.rect(leftX, topY, rightX - leftX, BELT_WIDTH)
  tint.fill({ color, alpha: 0.1 })
  parent.addChild(tint)

  // Top edge highlight (reads as "light hitting the near rail").
  const top = new Graphics()
  top.moveTo(leftX, topY)
  top.lineTo(rightX, topY)
  top.stroke({ width: 1.25, color: 0xffffff, alpha: 0.95 })
  parent.addChild(top)

  // Bottom edge shadow (far rail, in shadow).
  const bot = new Graphics()
  bot.moveTo(leftX, botY)
  bot.lineTo(rightX, botY)
  bot.stroke({ width: 1.25, color: 0x000000, alpha: 0.18 })
  parent.addChild(bot)

  // Outer end-cap — a short accent-colored band marking the dock point.
  const capX = direction === 1 ? outerX - 3 : outerX
  const cap = new Graphics()
  cap.rect(capX, topY + 2, 3, BELT_WIDTH - 4)
  cap.fill({ color, alpha: 0.45 })
  parent.addChild(cap)
}

// Ownership tints for pile chips — warm terracotta for entities the
// session user authored, cooler slate-blue for entities authored by
// others. Mirrors the on-belt item tinting in scene.ts so a PR's color
// is consistent whether it's traveling or piled.
const TINT_MINE_HEX = 0xc47a5a
const TINT_OTHER_HEX = 0x7a9aad

function drawPileChip(
  parent: Container,
  cx: number,
  cy: number,
  w: number,
  h: number,
  mine: boolean,
) {
  // Each pile chip is a small luminous data block: rounded rect with a
  // mine/other tinted body, top highlight (light catches the upper
  // edge), and a 1px bottom shadow (the chip sits on the chamber
  // floor). Sized small enough that 12 of them fit in the chamber's
  // bottom half without crowding the watermark glyph.
  const tint = mine ? TINT_MINE_HEX : TINT_OTHER_HEX
  const r = 3

  // Body — translucent tint over the chamber floor for a "lit data
  // block sitting in the recess" feel.
  const body = new Graphics()
  body.roundRect(cx, cy, w, h, r)
  body.fill({ color: tint, alpha: 0.85 })
  parent.addChild(body)

  // Inner gradient simulated as a brighter top half rectangle.
  const topHalf = new Graphics()
  topHalf.roundRect(cx + 0.5, cy + 0.5, w - 1, h / 2, r - 1)
  topHalf.fill({ color: 0xffffff, alpha: 0.18 })
  parent.addChild(topHalf)

  // Top highlight — sells the "rounded top edge catches light."
  const top = new Graphics()
  top.moveTo(cx + r, cy + 0.5)
  top.lineTo(cx + w - r, cy + 0.5)
  top.stroke({ width: 1, color: 0xffffff, alpha: 0.85 })
  parent.addChild(top)

  // Hairline outer rim for definition against the chamber floor.
  const rim = new Graphics()
  rim.roundRect(cx, cy, w, h, r)
  rim.stroke({ width: 0.75, color: 0x000000, alpha: 0.18, alignment: 1 })
  parent.addChild(rim)
}

function drawPileOverflow(
  parent: Container,
  cx: number,
  cy: number,
  w: number,
  h: number,
  count: number,
  color: number,
) {
  // Overflow tile takes the last visible slot when the pile would
  // exceed PILE_VISIBLE_CAP. Reads as "+N more" so the user sees the
  // count is bigger than what's drawn; the full list is in the near-
  // zoom HTML overlay.
  const r = 3
  const body = new Graphics()
  body.roundRect(cx, cy, w, h, r)
  body.fill({ color: 0xffffff, alpha: 0.92 })
  body.stroke({ width: 0.75, color, alpha: 0.4 })
  parent.addChild(body)

  const text = new Text({
    text: `+${count}`,
    resolution: 3,
    style: {
      fontFamily: 'Inter, system-ui, sans-serif',
      fontSize: 8,
      fontWeight: '700',
      fill: color,
      letterSpacing: 0.2,
    },
  })
  text.anchor.set(0.5, 0.5)
  text.x = cx + w / 2
  text.y = cy + h / 2 + 0.5
  parent.addChild(text)
}
