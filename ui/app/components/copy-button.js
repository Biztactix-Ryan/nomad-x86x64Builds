import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { task, timeout } from 'ember-concurrency';

export default class CopyButton extends Component {
  @tracked state = null;

  @(task(function* () {
    this.state = 'success';

    yield timeout(2000);
    this.state = null;
  }).restartable())
  indicateSuccess;
}
