import { describe, it, expect } from "vitest";
import { failureMessage } from "./failure";

describe("failure messages", () => {
  it("uses Warden's JSON error", () => {
    expect(failureMessage(400, "Bad Request", '{"error":"invalid mode"}')).toBe(
      "invalid mode",
    );
  });
  it("uses a plain text body", () => {
    expect(failureMessage(503, "", "Warden host is offline\n")).toBe(
      "Warden host is offline",
    );
  });
  it("reduces an ingress error page to its title", () => {
    const page =
      "<html>\r\n<head><title>503 Service Temporarily Unavailable</title></head>\r\n" +
      "<body>\r\n<center><h1>503 Service Temporarily Unavailable</h1></center>\r\n" +
      "<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n";
    expect(failureMessage(503, "", page)).toBe(
      "503 Service Temporarily Unavailable",
    );
    expect(failureMessage(502, "Bad Gateway", "<!DOCTYPE html><p>x</p>")).toBe(
      "502 Bad Gateway",
    );
  });
  it("falls back to the status line", () => {
    expect(failureMessage(504, "Gateway Timeout", "")).toBe(
      "504 Gateway Timeout",
    );
    expect(failureMessage(500, "", "[]")).toBe("[]");
  });
});
