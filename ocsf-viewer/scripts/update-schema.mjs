// Offline display dictionary. No event data is sent to the schema service.
import { writeFile } from 'node:fs/promises';
const version = '1.6.0';
const base = `https://schema.ocsf.io/api/${version}`;
async function read(path) {
  const response = await fetch(`${base}/${path}`);
  if (!response.ok) throw new Error(`${path}: HTTP ${response.status}`);
  return response.json();
}
const classes = await read('classes');
const catalog = {
  version,
  source: 'https://github.com/ocsf/ocsf-schema/tree/v1.6.0',
  classes: {},
};
let index = 0;
await Promise.all(
  Array.from({ length: 4 }, async () => {
    while (index < classes.length) {
      const item = classes[index++];
      const detail = await read(
        `classes/${item.extension ? `${item.extension}/` : ''}${item.name}`,
      );
      const attributes = Object.assign({}, ...detail.attributes);
      catalog.classes[item.uid] = {
        name: item.name,
        caption: item.caption,
        category: item.category_name,
        category_uid: item.category_uid,
        attributes: Object.fromEntries(
          Object.entries(attributes).map(([key, value]) => [
            key,
            {
              caption: value.caption,
              type: value.type,
              group: value.group,
              requirement: value.requirement,
              profile: value.profile,
              description: value.description
                ?.replace(/<[^>]*>/g, ' ')
                .replace(/\s+/g, ' ')
                .trim(),
              enum: value.enum
                ? Object.fromEntries(
                    Object.entries(value.enum).map(([id, entry]) => [
                      id,
                      entry.caption,
                    ]),
                  )
                : undefined,
            },
          ]),
        ),
      };
    }
  }),
);
await writeFile(
  new URL('../lib/catalog.json', import.meta.url),
  JSON.stringify(catalog),
);
console.log(
  `Saved ${Object.keys(catalog.classes).length} OCSF ${version} classes.`,
);

await import('./sync-contract.mjs');
