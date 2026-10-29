import {copyFile, mkdir} from 'node:fs/promises';
await mkdir(new URL('./vendor/', import.meta.url), {recursive: true});
await copyFile(new URL('./node_modules/keycloak-js/lib/keycloak.js', import.meta.url), new URL('./vendor/keycloak.js', import.meta.url));
await copyFile(new URL('./node_modules/keycloak-js/lib/deprecations.js', import.meta.url), new URL('./vendor/deprecations.js', import.meta.url));
await copyFile(new URL('./node_modules/keycloak-js/LICENSE.txt', import.meta.url), new URL('./vendor/KEYCLOAK-LICENSE', import.meta.url));
