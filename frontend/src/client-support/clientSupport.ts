export const MINIMUM_VIEWPORT_WIDTH = 1280;
export const MINIMUM_VIEWPORT_HEIGHT = 720;

export type BrowserFamily = "chrome" | "edge" | "obs" | "unknown";
export type UnsupportedReason = "browser" | "device" | "viewport";

export interface ClientSnapshot {
  userAgent: string;
  userAgentDataMobile: boolean | null;
  maxTouchPoints: number;
  obsStudioIntegration: boolean;
  viewportWidth: number;
  viewportHeight: number;
}

export interface ClientSupportClassification {
  supported: boolean;
  browserFamily: BrowserFamily;
  reasons: UnsupportedReason[];
}

const numericProductVersion = String.raw`\d+(?:\.\d+){0,3}`;
const chromiumPrefix =
  String.raw`Mozilla\/5\.0 \([^)]+\) AppleWebKit\/537\.36 ` +
  String.raw`\(KHTML, like Gecko\) Chrome\/${numericProductVersion} Safari\/537\.36`;
const chromeUserAgent = new RegExp(`^${chromiumPrefix}$`);
const edgeUserAgent = new RegExp(
  `^${chromiumPrefix} Edg/${numericProductVersion}$`,
);
const obsUserAgent = new RegExp(
  String.raw`^Mozilla\/5\.0 \([^)]+\) AppleWebKit\/537\.36 ` +
    String.raw`\(KHTML, like Gecko\) Chrome\/${numericProductVersion} ` +
    String.raw`OBS\/${numericProductVersion} Safari\/537\.36$`,
);
const mobileOrTabletUserAgent =
  /Android|Mobi|Mobile|Tablet|iPad|iPhone|iPod|Silk|Kindle/i;

function classifyBrowser(snapshot: ClientSnapshot): BrowserFamily {
  if (snapshot.obsStudioIntegration) {
    return obsUserAgent.test(snapshot.userAgent) ? "obs" : "unknown";
  }

  if (edgeUserAgent.test(snapshot.userAgent)) {
    return "edge";
  }

  if (chromeUserAgent.test(snapshot.userAgent)) {
    return "chrome";
  }

  return "unknown";
}

function isMobileOrTablet(snapshot: ClientSnapshot): boolean {
  if (snapshot.userAgentDataMobile === true) {
    return true;
  }

  if (mobileOrTabletUserAgent.test(snapshot.userAgent)) {
    return true;
  }

  return snapshot.maxTouchPoints > 1 && /Macintosh/i.test(snapshot.userAgent);
}

function hasSupportedViewport(snapshot: ClientSnapshot): boolean {
  return (
    Number.isFinite(snapshot.viewportWidth) &&
    Number.isFinite(snapshot.viewportHeight) &&
    snapshot.viewportWidth >= MINIMUM_VIEWPORT_WIDTH &&
    snapshot.viewportHeight >= MINIMUM_VIEWPORT_HEIGHT
  );
}

export function classifyClientSupport(
  snapshot: ClientSnapshot,
): ClientSupportClassification {
  const browserFamily = classifyBrowser(snapshot);
  const reasons: UnsupportedReason[] = [];

  if (browserFamily === "unknown") {
    reasons.push("browser");
  }

  if (isMobileOrTablet(snapshot)) {
    reasons.push("device");
  }

  if (!hasSupportedViewport(snapshot)) {
    reasons.push("viewport");
  }

  return {
    supported: reasons.length === 0,
    browserFamily,
    reasons,
  };
}
