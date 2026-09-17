// The math chunk: remark-math, rehype-katex, KaTeX and its stylesheet (whose
// fonts Vite copies into the build), loaded by RichText the first time a
// message has math. Nothing in the main chunk imports this file.
import "katex/dist/katex.min.css";
import rehypeKatex from "rehype-katex";
import remarkMath from "remark-math";
import type { Options } from "react-markdown";
import { KATEX_OPTIONS } from "./math";

export type MathPlugins = {
  remark: NonNullable<Options["remarkPlugins"]>;
  rehype: NonNullable<Options["rehypePlugins"]>;
};

export const plugins: MathPlugins = {
  remark: [remarkMath],
  rehype: [[rehypeKatex, KATEX_OPTIONS]],
};
