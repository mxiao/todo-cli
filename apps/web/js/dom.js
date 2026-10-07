// Tiny DOM builder. Text is always inserted as text nodes, never as HTML,
// so task titles and notes cannot inject markup.

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'value') el.value = v;
      else if (k === 'checked' || k === 'disabled' || k === 'hidden' || k === 'selected') el[k] = !!v;
      else el.setAttribute(k, v === true ? '' : String(v));
    }
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

// options fills a <select>. When the choices are unchanged only the value is
// set, so an open dropdown is not rebuilt under the user's pointer.
export function options(select, items, current) {
  const same = select.options.length === items.length &&
    items.every((it, i) => select.options[i].value === it.value && select.options[i].textContent === it.label);
  if (same) {
    select.value = current;
    return;
  }
  clear(select);
  for (const it of items) select.append(h('option', { value: it.value, selected: it.value === current }, it.label));
  select.value = current;
}
