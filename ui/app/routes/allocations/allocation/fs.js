import Route from '@ember/routing/route';
import notifyError from 'nomad-ui/utils/notify-error';

export default class FsRoute extends Route {
  async model({ path = '/' }) {
    const decodedPath = decodeURIComponent(path);
    const allocation = this.modelFor('allocations.allocation');

    if (!allocation) return;

    try {
      const [statJson] = await Promise.all([
        allocation.stat(decodedPath),
        allocation.get('node'),
      ]);

      if (statJson.IsDir) {
        return {
          path: decodedPath,
          allocation,
          directoryEntries: await allocation.ls(decodedPath),
          isFile: false,
        };
      } else {
        return {
          path: decodedPath,
          allocation,
          isFile: true,
          stat: statJson,
        };
      }
    } catch (e) {
      notifyError.call(this, e);
    }
  }

  setupController(
    controller,
    { path, allocation, directoryEntries, isFile, stat } = {}
  ) {
    super.setupController(...arguments);
    controller.setProperties({
      path,
      allocation,
      directoryEntries,
      isFile,
      stat,
    });
  }
}
