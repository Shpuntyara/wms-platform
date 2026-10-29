export class CommandJournal {
  constructor({storage, namespace, request, uuid = () => crypto.randomUUID()}) {
    Object.assign(this, {storage, namespace, request, uuid});
  }
  pending() {
    const raw = this.storage.getItem('wms-command:' + this.namespace);
    if (!raw) return null;
    const entry = JSON.parse(raw);
    if (typeof entry.id !== 'string' || !entry.path?.startsWith('/api/v1/') || !entry.body || typeof entry.body !== 'object') throw new Error('Nie udało się odczytać niepotwierdzonej operacji. Skontaktuj się z kierownikiem zmiany');
    return entry;
  }
  async submit(path, body) {
    if (this.sending || this.pending()) throw new Error('Najpierw sprawdź wynik poprzedniej operacji');
    const entry = {id: this.uuid(), path, body: structuredClone(body)};
    try { this.storage.setItem('wms-command:' + this.namespace, JSON.stringify(entry)); }
    catch { throw new Error('Nie udało się zapisać operacji na urządzeniu. Żądanie nie zostało wysłane'); }
    return this.send(entry, false);
  }
  async retry() {
    if (this.sending) throw new Error('Trwa sprawdzanie operacji');
    const entry = this.pending();
    if (!entry) throw new Error('Brak operacji do sprawdzenia');
    return this.send(entry, true);
  }
  async send(entry, recovering) {
    this.sending = true;
    try {
      const result = await this.request(entry.path, entry.body, entry.id);
      this.storage.removeItem('wms-command:' + this.namespace);
      return result;
    } catch (err) {
      // 401 may happen during a refresh; server/transport failures can hide COMMIT.
      if (!recovering && [400, 403, 404, 409, 413, 415, 422].includes(err.status)) this.storage.removeItem('wms-command:' + this.namespace);
      throw err;
    } finally { this.sending = false; }
  }
}
