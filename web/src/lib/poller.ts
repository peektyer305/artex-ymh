export interface Poller<T> {
  refresh: () => Promise<void>;
  stop: () => void;
}

// Every trigger shares the same in-flight request. A stop aborts it and blocks its result.
export function createPoller<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  receive: (value: T) => void,
  interval: number,
  onError: (error: unknown) => void = () => {},
): Poller<T> {
  let stopped = false;
  let controller: AbortController | undefined;
  let running: Promise<void> | undefined;
  const refresh = (): Promise<void> => {
    if (stopped) return Promise.resolve();
    if (running) return running;
    controller = new AbortController();
    const signal = controller.signal;
    running = Promise.resolve().then(() => fetcher(signal)).then((value) => {
      if (!stopped && !signal.aborted) receive(value);
    }).catch((error: unknown) => {
      if (!stopped && !signal.aborted) onError(error);
    }).finally(() => { running = undefined; });
    return running;
  };
  const timer = interval > 0 ? setInterval(() => void refresh(), interval) : undefined;
  return {
    refresh,
    stop: () => { stopped = true; if (timer !== undefined) clearInterval(timer); controller?.abort(); },
  };
}
