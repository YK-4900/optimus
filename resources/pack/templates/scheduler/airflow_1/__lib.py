import os
import json
import calendar
import re
from typing import Any, Callable, Dict, Optional
from datetime import datetime, timedelta, timezone
from string import Template
from croniter import croniter

from airflow.kubernetes import kube_client, pod_launcher
from airflow.models import DAG, Variable, DagRun, DagModel, TaskInstance, BaseOperator, XCom, XCOM_RETURN_KEY
from airflow.hooks.http_hook import HttpHook
from airflow.operators.python_operator import PythonOperator
from airflow.sensors.base_sensor_operator import BaseSensorOperator
from airflow.contrib.operators.kubernetes_pod_operator import KubernetesPodOperator
from airflow.kubernetes.secret import Secret
from airflow.sensors.external_task_sensor import ExternalTaskSensor
from airflow.exceptions import AirflowException
from airflow.utils.decorators import apply_defaults
from airflow.utils.db import provide_session
from airflow.configuration import conf
from airflow.utils.state import State
from airflow.hooks.base_hook import BaseHook


class SuperKubernetesPodOperator(KubernetesPodOperator):
    """
    ** SAME AS KubernetesPodOperator: Execute a task in a Kubernetes Pod **
    Wrapper to push xcom as a return value key even if container completes with non success status

    .. note: keep this up to date if there is any change in KubernetesPodOperator execute method
    """
    template_fields = ('image', 'cmds', 'arguments', 'env_vars', 'config_file', 'pod_template_file')

    @apply_defaults
    def __init__(self,
                *args,
                **kwargs):
        super(SuperKubernetesPodOperator, self).__init__(*args, **kwargs)

        self.do_xcom_push = kwargs.get('do_xcom_push')
        self.namespace = kwargs.get('namespace')
        self.in_cluster = kwargs.get('in_cluster')
        self.cluster_context = kwargs.get('cluster_context')
        self.reattach_on_restart = kwargs.get('reattach_on_restart')
        self.config_file = kwargs.get('config_file')

    def execute(self, context):
        try:
            if self.in_cluster is not None:
                client = kube_client.get_kube_client(in_cluster=self.in_cluster,
                                                     cluster_context=self.cluster_context,
                                                     config_file=self.config_file)
            else:
                client = kube_client.get_kube_client(cluster_context=self.cluster_context,
                                                     config_file=self.config_file)

            # Add combination of labels to uniquely identify a running pod
            labels = self.create_labels_for_pod(context)

            label_selector = self._get_pod_identifying_label_string(labels)

            pod_list = client.list_namespaced_pod(self.namespace, label_selector=label_selector)

            if len(pod_list.items) > 1 and self.reattach_on_restart:
                raise AirflowException(
                    'More than one pod running with labels: '
                    '{label_selector}'.format(label_selector=label_selector))

            launcher = pod_launcher.PodLauncher(kube_client=client, extract_xcom=self.do_xcom_push)

            if len(pod_list.items) == 1:
                try_numbers_match = self._try_numbers_match(context, pod_list.items[0])
                final_state, result = self.handle_pod_overlap(labels, try_numbers_match, launcher, pod_list)
            else:
                final_state, _, result = self.create_new_pod_for_operator(labels, launcher)

            if final_state != State.SUCCESS:
                # push xcom value even if pod fails
                context.get('task_instance').xcom_push(key=XCOM_RETURN_KEY, value=result)
                raise AirflowException(
                    'Pod returned a failure: {state}'.format(state=final_state))
            return result
        except AirflowException as ex:
            raise AirflowException('Pod Launching failed: {error}'.format(error=ex))


class SuperExternalTaskSensor(BaseSensorOperator):
    """
    Waits for a different DAG or a task in a different DAG to complete for a
    specific execution window

    :param external_dag_id: The dag_id that contains the task you want to
        wait for
    :type external_dag_id: str
    :param allowed_states: list of allowed states, default is ``['success']``
    :type allowed_states: list
    :param window_size: size of the window in hours to look for successful 
        runs in upstream dag. E.g, "24" will check for last 24 hours from
        current execution date of this dag. It checks for number of successful
        iterations of upstream dag in provided window. All of them needs to be
        successful for this sensor to complete. Defaults to a day of window(24)
    :type window_size: int
    """

    @apply_defaults
    def __init__(self, 
                external_dag_id,
                window_size: int,
                window_offset: int,
                window_truncate_upto: str,
                *args, 
                **kwargs):

        # Sensor's have two mode of operations: 'poke' and 'reschedule'. 'poke'
        # mode is like having a while loop. when the scheduler runs the task, the
        # sensor keeps checking for predicate condition until it becomes true. This
        # has the effect that once a sensor starts, it keeps taking resources until 
        # it senses that the predicate has been met. when set to 'reschedule' it exits
        # immediately if the predicate is false and is scheduled at a later time.
        # see the documentation for BaseSensorOperator for more information
        kwargs['mode'] = kwargs.get('mode', 'reschedule')
        
        self.upstream_dag = external_dag_id
        self.window_size = window_size
        self.window_offset = window_offset
        self.window_truncate_upto = window_truncate_upto
        self.allowed_upstream_states = [State.SUCCESS]

        super(SuperExternalTaskSensor, self).__init__(*args, **kwargs)

    @provide_session
    def poke(self, context, session=None):

        dag_to_wait = session.query(DagModel).filter(
            DagModel.dag_id == self.upstream_dag
        ).first()

        # check if valid upstream dag
        if not dag_to_wait:
            raise AirflowException('The external DAG '
                                    '{} does not exist.'.format(self.upstream_dag))
        else:
            if not os.path.exists(dag_to_wait.fileloc):
                raise AirflowException('The external DAG '
                                        '{} was deleted.'.format(self.upstream_dag))

        # calculate windows
        execution_date = context['execution_date']
        window_start, window_end = self.generate_window(execution_date, self.window_size, self.window_offset, self.window_truncate_upto)
        self.log.info("consuming upstream window between: {} - {}".format(window_start.isoformat(), window_end.isoformat()))

        # find success iterations we need in window
        expected_upstream_executions = []
        dag_cron = croniter(dag_to_wait.schedule_interval, window_start.replace(tzinfo=None))
        while True:
            next_run = dag_cron.get_next(datetime)
            if next_run > window_end.replace(tzinfo=None):
                break
            expected_upstream_executions.append(next_run)
        self.log.info("expected upstream executions ({}): {}".format(len(expected_upstream_executions), expected_upstream_executions))

        # upstream dag runs between input window with success state
        actual_upstream_executions = [ r.execution_date for r in session.query(DagRun.execution_date)
            .filter(
                DagRun.dag_id == self.upstream_dag,
                DagRun.execution_date > window_start,
                DagRun.execution_date <= window_end,
                DagRun.external_trigger == False,
                DagRun.state.in_(self.allowed_upstream_states)
            ).order_by(DagRun.execution_date).all() ]
        self.log.info("actual upstream executions ({}): {}".format(len(actual_upstream_executions), actual_upstream_executions))

        missing_upstream_executions = set(expected_upstream_executions) - set(actual_upstream_executions)
        if len(missing_upstream_executions) > 0:
            self.log.info("missing upstream executions : {}".format(missing_upstream_executions))
            self.log.warning(
                "unable to find enough DagRun instances for upstream '{}' dated between {} and {}(inclusive), rescheduling sensor"
                    .format(self.upstream_dag, window_start.isoformat(), window_end.isoformat()))
            return False

        return True

    def generate_window(self, end_time, window_size, window_offset, window_truncate_upto):
        floating_end = end_time

        # apply truncation
        if window_truncate_upto == "w":
            # remove time upto days and find nearest week
            # get week lists for current month
            week_matrix_per_month = calendar.Calendar().monthdatescalendar(end_time.year, end_time.month)
            # find week where current day lies
            current_week = None
            for week in week_matrix_per_month:
                for day in week:
                    if day == end_time.date():
                        current_week = week

            floating_end = datetime.combine(current_week[6], end_time.timetz())
            floating_end = floating_end.replace(tzinfo=timezone.utc)

        end = floating_end #+ timedelta(seconds=window_offset * 60 * 60)
        start = end - timedelta(seconds=window_size * 60 * 60)
        return start, end


class SlackWebhookOperator(BaseOperator):
    """
    This operator allows you to post messages to Slack using incoming webhooks.
    Takes both Slack webhook token directly and connection that has Slack webhook token.
    If both supplied, http_conn_id will be used as base_url,
    and webhook_token will be taken as endpoint, the relative path of the url.
    Each Slack webhook token can be pre-configured to use a specific channel, username and
    icon. You can override these defaults in this hook.
    :param http_conn_id: connection that has Slack webhook token in the extra field
    :type http_conn_id: str
    :param webhook_token: Slack webhook token
    :type webhook_token: str
    :param message: The message you want to send on Slack
    :type message: str
    :param blocks: The blocks to send on Slack. Should be a list of
        dictionaries representing Slack blocks.
    :type blocks: list
    """

    template_fields = ['webhook_token', 'message', 'blocks']

    @apply_defaults
    def __init__(self,
                 http_conn_id=None,
                 webhook_token=None,
                 message="",
                 blocks=None,
                 *args,
                 **kwargs):
        super(SlackWebhookOperator, self).__init__(*args, **kwargs)

        self.http_conn_id = http_conn_id
        self.webhook_token = self._get_token(webhook_token, http_conn_id)
        self.message = message
        self.blocks = blocks
        self.hook = None

    def _get_token(self, token, http_conn_id):
        """
        Given either a manually set token or a conn_id, return the webhook_token to use.
        :param token: The manually provided token
        :type token: str
        :param http_conn_id: The conn_id provided
        :type http_conn_id: str
        :return: webhook_token to use
        :rtype: str
        """
        if token:
            return token
        elif http_conn_id:
            conn = self.get_connection(http_conn_id)
            extra = conn.extra_dejson
            return extra.get('webhook_token', '')
        else:
            raise AirflowException('Cannot get token: No valid Slack connection')

    def _build_slack_message(self):
        """
        Construct the Slack message. All relevant parameters are combined here to a valid
        Slack json message.
        :return: Slack message to send
        :rtype: str
        """
        cmd={}
        if self.blocks:
            cmd['blocks']=self.blocks
        cmd['text']=self.message
        return json.dumps(cmd)

    def execute(self, context):
        slack_message=self._build_slack_message()
        self.log.info("sending alert to slack")
        self.log.info(slack_message)

        self.hook=HttpHook(http_conn_id=self.http_conn_id)
        response=self.hook.run(
            self.webhook_token,
            data=slack_message,
            headers={'Content-type': 'application/json'}
        )

        if response.status_code == 200:
            return response.text
        raise AirflowException("failed to send slack alert: {}".format(response.text))


def alert_failed_to_slack(context):
    SLACK_CONN_ID = "slack_alert"
    TASKFAIL_ALERT = int(Variable.get("taskfail_alert", default_var=1))

    def _xcom_value_has_error(_xcom) -> bool:
        return _xcom.key == XCOM_RETURN_KEY and isinstance(_xcom.value, dict) and 'error' in _xcom.value and _xcom.value['error'] != None

    if TASKFAIL_ALERT != 1:
        return "suppressed failure alert"

    slack_token = ""
    try:
        slack_token = BaseHook.get_connection(SLACK_CONN_ID).password
    except:
        print("no slack connection variable set")
        return "{connection} connection variable not defined, unable to send alerts".format(connection=SLACK_CONN_ID)
    
    current_dag_id = context.get('task_instance').dag_id
    current_task_id = context.get('task_instance').task_id
    current_execution_date = context.get('execution_date')

    # failure message pushed by failed tasks
    failure_messages = []
    for xcom in XCom.get_many(
                current_execution_date,
                key=None,
                task_ids=None,
                dag_ids=current_dag_id,
                include_prior_dates=False,
                limit=10):
        if xcom.key == 'error':
            failure_messages.append(xcom.value)
        if _xcom_value_has_error(xcom):
            failure_messages.append(xcom.value['error'])
    failure_message = ", ".join(failure_messages)
    print("failures: {}".format(failure_message))

    message_body = "\n".join([
        "• *DAG*: {}".format(current_dag_id),
        "• *Task*: {}".format(current_task_id),
        "• *Execution Time*: {}".format(current_execution_date),
        "• *Run ID*: {}".format(context.get('run_id'))
    ])

    message_footer = "\n".join([
        ":blob-facepalm: Owner: {}".format(context.get('dag').owner),
        ":hourglass: Duration: {} sec".format(context.get('task_instance').duration),
        ":memo: Details: {}".format(failure_message)
    ])

    blocks = [
        {
            "type": "section",
            "text": {
                "type": "mrkdwn",
                "text": "Task failed :fire:"
            }
        },
        {
            "type": "section",
            "text": {
                "type": "mrkdwn",
                "text": message_body
            }
        },
        {
            "type": "actions",
            "elements": [
                {
                    "type": "button",
                    "style": "danger",
                    "text": {
                        "type": "plain_text",
                        "text": "View log :airflow:",
                    },
                    "url": context.get('task_instance').log_url,
                    "action_id": "view_log",
                }
            ]
        },
        {
            "type": "divider"
        },
        {
            "type": "context",
            "elements": [
                {
                    "type": "mrkdwn",
                    "text": message_footer
                },
            ]
        },
    ]
    failed_alert = SlackWebhookOperator(
        task_id='slack_failed_alert',
        http_conn_id=SLACK_CONN_ID,
        webhook_token=slack_token,
        blocks=blocks,
    )
    return failed_alert.execute(context=context)