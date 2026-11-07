import type { ReactElement } from "react";

// There are no product photos, and a grey box with an icon says "unfinished".
// Instead each product gets a small generated composition, derived from its id,
// so it is always the same for the same product and different from its
// neighbours. Two colours from the palette, nothing else.

function seeded(id: string) {
  let h = 2166136261;
  for (let i = 0; i < id.length; i++) h = Math.imul(h ^ id.charCodeAt(i), 16777619);
  return () => {
    h = Math.imul(h ^ (h >>> 15), 2246822507);
    h = Math.imul(h ^ (h >>> 13), 3266489909);
    h ^= h >>> 16;
    return (h >>> 0) / 4294967296;
  };
}

const INK = { fill: "rgb(var(--ink))" };
const BRAND = { fill: "rgb(var(--brand))" };
const PAPER = { fill: "rgb(var(--surface))" };
const SUNKEN = { fill: "rgb(var(--sunken))" };

export function ProductArt({ id, className = "" }: { id: string; className?: string }) {
  const r = seeded(id);
  const kind = Math.floor(r() * 4);
  const shapes: ReactElement[] = [];

  if (kind === 0) {
    // concentric rings, off-centre
    const cx = 40 + r() * 80;
    const cy = 30 + r() * 40;
    for (let i = 5; i >= 1; i--) shapes.push(<circle key={i} cx={cx} cy={cy} r={i * 14} style={i % 2 ? INK : SUNKEN} />);
    shapes.push(<circle key="dot" cx={cx} cy={cy} r={6} style={BRAND} />);
  } else if (kind === 1) {
    // vertical bars of varying width
    let x = 0;
    for (let i = 0; x < 160; i++) {
      const w = 6 + r() * 16;
      shapes.push(<rect key={i} x={x} y={0} width={w} height={100} style={i % 3 === 0 ? INK : i % 3 === 1 ? SUNKEN : PAPER} />);
      x += w;
    }
    shapes.push(<rect key="b" x={20 + r() * 100} y={20 + r() * 40} width={26} height={26} style={BRAND} />);
  } else if (kind === 2) {
    // a grid of quarter-circles
    const n = 5;
    for (let row = 0; row < 3; row++)
      for (let col = 0; col < n; col++) {
        const x = col * 32;
        const y = row * 33.4;
        const flip = r() > 0.5;
        const d = flip ? `M${x} ${y}h32a32 32 0 0 1-32 32z` : `M${x + 32} ${y + 32}h-32a32 32 0 0 1 32-32z`;
        shapes.push(<path key={`${row}${col}`} d={d} style={r() > 0.8 ? BRAND : (row + col) % 2 ? INK : SUNKEN} />);
      }
  } else {
    // stacked horizontal slabs, one of them the accent
    const hot = Math.floor(r() * 4);
    for (let i = 0; i < 4; i++) {
      const w = 60 + r() * 100;
      shapes.push(<rect key={i} x={i % 2 ? 160 - w : 0} y={i * 25} width={w} height={22} style={i === hot ? BRAND : i % 2 ? SUNKEN : INK} />);
    }
  }

  return (
    <svg viewBox="0 0 160 100" preserveAspectRatio="xMidYMid slice" className={className} role="img" aria-label="" aria-hidden="true">
      <rect width="160" height="100" style={PAPER} />
      {shapes}
    </svg>
  );
}
