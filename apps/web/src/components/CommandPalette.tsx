import { useEffect, useMemo, useRef, useState } from 'react';
import { fuzzyScore } from '../lib/insights';

export interface PaletteItem {
  id: string;
  group: 'Go to' | 'Components' | 'Actions' | 'Layers';
  label: string;
  hint?: string;
  color?: string;
  keywords?: string;
  run: () => void;
}

interface Props {
  open: boolean;
  items: PaletteItem[];
  onClose: () => void;
}

export function CommandPalette({ open, items, onClose }: Props) {
  const [q, setQ] = useState('');
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (open) {
      setQ('');
      setActive(0);
      setTimeout(() => inputRef.current?.focus(), 0);
    }
  }, [open]);

  const results = useMemo(() => {
    const scored = items
      .map((it) => ({ it, s: fuzzyScore(q, `${it.label} ${it.hint ?? ''} ${it.keywords ?? ''}`) }))
      // Substring hits score 50+; scattered-letter hits must mostly land on word starts.
      .filter((x) => x.s > 0 && (x.s >= 50 || x.s >= q.trim().length * 4));
    if (q) scored.sort((a, b) => b.s - a.s);
    return scored.slice(0, 40).map((x) => x.it);
  }, [items, q]);

  useEffect(() => setActive(0), [q]);
  useEffect(() => {
    listRef.current?.querySelector<HTMLElement>(`[data-idx="${active}"]`)?.scrollIntoView({ block: 'nearest' });
  }, [active]);

  if (!open) return null;
  const run = (it: PaletteItem | undefined) => {
    if (!it) return;
    onClose();
    it.run();
  };
  let lastGroup = '';

  return (
    <div className="palette-backdrop" onMouseDown={onClose}>
      <div className="palette" role="dialog" aria-modal="true" aria-label="Command palette" onMouseDown={(e) => e.stopPropagation()}>
        <input
          ref={inputRef}
          className="palette-input"
          placeholder="Search components, sections and actions…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') { e.preventDefault(); setActive((a) => Math.min(results.length - 1, a + 1)); }
            else if (e.key === 'ArrowUp') { e.preventDefault(); setActive((a) => Math.max(0, a - 1)); }
            else if (e.key === 'Enter') { e.preventDefault(); run(results[active]); }
            else if (e.key === 'Escape') { e.preventDefault(); onClose(); }
          }}
          aria-activedescendant={`pal-${active}`}
        />
        <div className="palette-list" ref={listRef} role="listbox">
          {results.length === 0 && <div className="empty-note palette-empty">Nothing matches “{q}”.</div>}
          {results.map((it, i) => {
            const header = !q && it.group !== lastGroup ? it.group : null;
            lastGroup = it.group;
            return (
              <div key={it.id}>
                {header && <div className="palette-group">{header}</div>}
                <button type="button" id={`pal-${i}`} data-idx={i} role="option" aria-selected={i === active}
                  className={`palette-item ${i === active ? 'active' : ''}`} onMouseEnter={() => setActive(i)} onClick={() => run(it)}>
                  {it.color && <span className="layer-swatch" style={{ background: it.color }} />}
                  <span className="palette-label">{it.label}</span>
                  {it.hint && <span className="palette-hint">{it.hint}</span>}
                </button>
              </div>
            );
          })}
        </div>
        <div className="palette-foot"><kbd>↑↓</kbd> move <kbd>↵</kbd> open <kbd>esc</kbd> close</div>
      </div>
    </div>
  );
}
