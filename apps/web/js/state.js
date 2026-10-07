// A minimal observable store: one state object, shallow-merged updates,
// subscribers called after every change.

export function createStore(initial) {
  let state = initial;
  const subs = new Set();
  return {
    get: () => state,
    set(patch) {
      const next = typeof patch === 'function' ? patch(state) : patch;
      state = { ...state, ...next };
      for (const fn of subs) fn(state);
    },
    subscribe(fn) {
      subs.add(fn);
      return () => subs.delete(fn);
    },
  };
}
