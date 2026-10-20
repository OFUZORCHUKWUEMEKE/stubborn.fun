// Vite's ?raw imports load a file's source as a string.
declare module "*?raw" {
  const source: string;
  export default source;
}
