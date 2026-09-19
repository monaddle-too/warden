import { describe, expect, it } from "vitest";
import { documentTitle, instanceLabel } from "./instance";

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
  });
});
