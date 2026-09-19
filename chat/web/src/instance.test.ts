import { describe, expect, it } from "vitest";
import { documentTitle, instanceLabel, signedOutHeading } from "./instance";

describe("instance", () => {
  it("names a non-default instance with its build", () => {
    expect(instanceLabel({ name: "dogfood-a", version: "v0.0.0-dev.abc" })).toBe(
      "dogfood-a v0.0.0-dev.abc",
    );
    expect(instanceLabel({ name: "dev", version: "" })).toBe("dev");
  });
  it("shows nothing new for the default instance", () => {
    expect(instanceLabel(undefined)).toBe("");
    expect(instanceLabel({ name: "default", version: "v1" })).toBe("");
    expect(documentTitle("Warden — Chats", undefined)).toBe("Warden — Chats");
  });
  it("puts the instance in the browser title", () => {
    expect(
      documentTitle("Warden — Chats", { name: "dogfood-a", version: "v0.0.0-dev.abc" }),
    ).toBe("Warden · dogfood-a v0.0.0-dev.abc — Chats");
    expect(documentTitle("Warden — Sign in", { name: "inner", version: "v1" })).toBe(
      "Warden · inner v1 — Sign in",
    );
  });
  /* The signed-out page: which Warden is asking, and the build it runs. */
  it("names the install on the signed-out page", () => {
    expect(signedOutHeading({ name: "inner", version: "v0.0.0-dev.abc123" })).toBe(
      "Warden inner · v0.0.0-dev.abc123",
    );
    expect(signedOutHeading({ name: "default", version: "v0.0.0-dev.abc" })).toBe(
      "Warden v0.0.0-dev.abc",
    );
    expect(signedOutHeading({ name: "inner", version: "" })).toBe("Warden inner");
  });
  it("says only Warden when the install reports nothing", () => {
    expect(signedOutHeading(undefined)).toBe("Warden");
    expect(signedOutHeading({ name: "", version: "" })).toBe("Warden");
  });
});
