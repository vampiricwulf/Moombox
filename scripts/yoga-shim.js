let _yoga = null;

export function __initYoga(yoga) {
  _yoga = yoga;
}

export default new Proxy({}, {
  get(_target, prop) {
    if (prop === '__initYoga') return __initYoga;
    if (!_yoga) throw new Error('Yoga not initialized');
    return _yoga[prop];
  },
});
