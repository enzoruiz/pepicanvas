import type { ClientSnapshot } from "./clientSupport";

interface NavigatorSnapshotSource {
  userAgent: string;
  maxTouchPoints?: number;
  userAgentData?: {
    mobile?: boolean;
  };
}

interface ViewportSnapshotSource {
  innerWidth: number;
  innerHeight: number;
}

export function readClientSnapshot(
  navigatorSource: NavigatorSnapshotSource = navigator,
  viewportSource: ViewportSnapshotSource = window,
): ClientSnapshot {
  const mobileHint = navigatorSource.userAgentData?.mobile;

  return {
    userAgent: navigatorSource.userAgent,
    userAgentDataMobile: typeof mobileHint === "boolean" ? mobileHint : null,
    maxTouchPoints:
      typeof navigatorSource.maxTouchPoints === "number"
        ? navigatorSource.maxTouchPoints
        : 0,
    viewportWidth: viewportSource.innerWidth,
    viewportHeight: viewportSource.innerHeight,
  };
}
